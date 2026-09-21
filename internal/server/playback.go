package server

import (
	"crypto/rand"
	"encoding/hex"
	"math"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/TokenCemetery/coach/internal/media"
	"github.com/TokenCemetery/coach/internal/state"
)

func randomSessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// bitrate estimates the source bitrate from the container size and duration.
// Emby reports a slightly higher figure than the sum of stream bitrates; Coach
// derives its own value rather than copying a formula it cannot verify.
func bitrate(item media.Item) int64 {
	if item.RunTimeTicks <= 0 || item.Size <= 0 {
		return 0
	}
	seconds := float64(item.RunTimeTicks) / 10000000
	rate := math.Ceil(float64(item.Size) * 8 / seconds)
	if rate >= float64(math.MaxInt64) {
		return math.MaxInt64
	}
	return int64(rate)
}

func (s *Server) playbackInfo(w http.ResponseWriter, r *http.Request, token string, session state.Session) {
	item, found := s.findItem(r.PathValue("item"))
	if !found || item.IsFolder() {
		fail(w, 404, "NotFound")
		return
	}
	body, err := readPlaybackRequest(w, r)
	if err != nil {
		fail(w, 400, "InvalidRequest")
		return
	}
	if body.UserId != nil && *body.UserId != "" && !strings.EqualFold(*body.UserId, session.UserID) {
		fail(w, 403, "Forbidden")
		return
	}
	if body.MediaSourceId != nil && *body.MediaSourceId != "" && *body.MediaSourceId != "mediasource_"+item.ID {
		fail(w, 404, "MediaSourceNotFound")
		return
	}
	video, audio := playbackStreams(item, body.AudioStreamIndex)
	if body.AudioStreamIndex != nil && audio == nil {
		fail(w, 400, "InvalidAudioStreamIndex")
		return
	}
	if body.SubtitleStreamIndex != nil && *body.SubtitleStreamIndex >= 0 && !hasSubtitle(item, *body.SubtitleStreamIndex) {
		fail(w, 400, "InvalidSubtitleStreamIndex")
		return
	}
	playSession := randomSessionID()
	streams := mediaStreams(item)
	source := mediaSource(item, streams)
	direct := body.supportsDirectStream(item, video, audio)
	source["SupportsDirectPlay"] = false
	source["SupportsDirectStream"] = direct
	source["SupportsTranscoding"] = false
	source["ItemId"] = item.ID
	source["Formats"] = []string{}
	source["RequiredHttpHeaders"] = object{}
	source["AddApiKeyToDirectStreamUrl"] = false
	source["Bitrate"] = bitrate(item)
	if audio != nil {
		source["DefaultAudioStreamIndex"] = audio.Index
	}
	source["DefaultSubtitleStreamIndex"] = -1
	response := object{"MediaSources": []object{source}, "PlaySessionId": playSession}
	if direct {
		source["DirectStreamUrl"] = directStreamURL(item, session.DeviceID, playSession, token)
	} else {
		response["ErrorCode"] = "NoCompatibleStream"
	}
	respond(w, 200, response)
}

// directStreamURL matches the relative form Emby returns: no "/emby" prefix,
// because the client passes this through apiClient.getUrl, which adds the
// prefix itself. Including it here yields "/emby/emby/..." and a dead URL.
//
// The access token is embedded, as the reference server does, because the URL
// is handed to a media element that cannot send an Authorization header. Coach
// never logs request URLs, so the token stays out of the logs.
func directStreamURL(item media.Item, deviceID, playSession, token string) string {
	query := url.Values{}
	query.Set("MediaSourceId", "mediasource_"+item.ID)
	query.Set("PlaySessionId", playSession)
	query.Set("Static", "true")
	query.Set("api_key", token)
	if deviceID != "" {
		query.Set("DeviceId", deviceID)
	}
	name := "stream"
	if item.Container != "" {
		name += "." + item.Container
	}
	return "/videos/" + item.ID + "/" + name + "?" + query.Encode()
}

func (s *Server) findItem(id string) (media.Item, bool) {
	if s.media == nil || id == "" {
		return media.Item{}, false
	}
	for _, items := range [][]media.Item{s.media.Items, s.media.Folders} {
		for _, item := range items {
			if item.ID == id {
				return item, true
			}
		}
	}
	return media.Item{}, false
}

// streamVideo serves the original file. http.ServeContent implements Range,
// If-Range, HEAD, 206 and 416 exactly, so Coach does not reimplement them, and
// the file is streamed rather than read into memory.
func (s *Server) streamVideo(w http.ResponseWriter, r *http.Request, token string, session state.Session) {
	// The route may carry a container suffix, as in /videos/{id}/stream.mp4.
	if !streamRoute(r.PathValue("stream")) {
		fail(w, 404, "NotFound")
		return
	}
	item, found := s.findItem(r.PathValue("item"))
	if !found || item.IsFolder() {
		fail(w, 404, "NotFound")
		return
	}
	file, err := s.media.Open(item)
	if err != nil {
		fail(w, 404, "MediaUnavailable")
		return
	}
	defer file.Close()
	serveMediaFile(w, r, file, item)
}

// playstateReport is the body Emby Web posts to the /Sessions/Playing family.
// Unknown fields are ignored: Coach only persists what it can act on.
type playstateReport struct {
	ItemId        string
	PositionTicks *int64
	PlaySessionId string
}

// reportPlaystate records progress. A report for an unknown item is refused
// rather than stored, so a stale client cannot grow the state file.
func (s *Server) reportPlaystate(event string) authenticated {
	return func(w http.ResponseWriter, r *http.Request, token string, session state.Session) {
		var body playstateReport
		if r.ContentLength != 0 && decodeJSON(w, r, &body) != nil {
			fail(w, 400, "InvalidRequest")
			return
		}
		if body.ItemId == "" {
			body.ItemId = value(r, "ItemId")
		}
		if body.PlaySessionId == "" {
			body.PlaySessionId = value(r, "PlaySessionId")
		}
		if len(body.PlaySessionId) > 128 {
			fail(w, 400, "InvalidRequest")
			return
		}
		item, found := s.findItem(body.ItemId)
		if !found || item.IsFolder() {
			fail(w, 404, "NotFound")
			return
		}
		position := int64(0)
		hasPosition := body.PositionTicks != nil
		if body.PositionTicks != nil {
			position = *body.PositionTicks
		} else if text := value(r, "PositionTicks"); text != "" {
			parsed, err := strconv.ParseInt(text, 10, 64)
			if err != nil || parsed < 0 {
				fail(w, 400, "InvalidRequest")
				return
			}
			position = parsed
			hasPosition = true
		}
		if position < 0 {
			fail(w, 400, "InvalidRequest")
			return
		}
		if item.RunTimeTicks > 0 {
			position = min(position, item.RunTimeTicks)
		}
		_, err := s.updateItemSession(token, item, func(st *state.ItemState, current *state.Session) {
			if !acceptPlayReport(current, item.ID, body.PlaySessionId, event) {
				return
			}
			if hasPosition {
				st.PositionTicks = position
			}
			st.LastPlayed = time.Now().UTC()
			switch event {
			case "start":
				st.PlayCount++
			case "stop":
				// Finishing the tail of a file counts as watched, and the
				// stored position is cleared so it does not resume at the end.
				if item.RunTimeTicks > 0 && st.PositionTicks >= item.RunTimeTicks-item.RunTimeTicks/10 {
					st.Played, st.PositionTicks = true, 0
				}
			}
		})
		s.changed(w, err)
	}
}

// ID-bearing reports use a durable 64-start retry window per client session.
// Only the newest start accepts progress/stop; a completed play cannot reopen.
// Legacy reports without an ID retain arrival-order semantics.
func acceptPlayReport(session *state.Session, itemID, playID, event string) bool {
	if playID == "" {
		return true
	}
	index := slices.IndexFunc(session.RecentPlays, func(p state.PlaybackRecord) bool { return p.ID == playID })
	if event == "start" {
		if index >= 0 {
			return false
		}
		if len(session.RecentPlays) >= 64 {
			session.RecentPlays = slices.Clone(session.RecentPlays[len(session.RecentPlays)-63:])
		}
		session.RecentPlays = append(session.RecentPlays, state.PlaybackRecord{ID: playID, ItemID: itemID})
		return true
	}
	if index < 0 || index != len(session.RecentPlays)-1 {
		return false
	}
	play := &session.RecentPlays[index]
	if play.Stopped || play.ItemID != itemID {
		return false
	}
	if event == "stop" {
		play.Stopped = true
	}
	return true
}

// setItemFlag backs the played and favourite toggles, which answer with the
// item's updated UserData rather than an empty body.
func (s *Server) setItemFlag(favorite, value bool) authenticated {
	return func(w http.ResponseWriter, r *http.Request, token string, session state.Session) {
		item, found := s.findItem(r.PathValue("item"))
		if !found {
			fail(w, 404, "NotFound")
			return
		}
		data, err := s.updateItem(token, item, func(st *state.ItemState) {
			if favorite {
				st.IsFavorite = value
				return
			}
			st.Played = value
			if value {
				st.PositionTicks = 0
				if st.PlayCount == 0 {
					st.PlayCount = 1
				}
			}
		})
		if err != nil {
			s.changed(w, err)
			return
		}
		respond(w, 200, data)
	}
}

// resumeItems lists items with meaningful stored progress, most recent first.
func (s *Server) resumeItems(limit int, token string) []object {
	snapshot := s.store.Snapshot()
	serverID := snapshot.ServerID
	candidates := []*media.Item{}
	if s.media != nil {
		for i := range s.media.Items {
			item := &s.media.Items[i]
			if resumable(snapshot.User.Items[item.ID], item.RunTimeTicks) {
				candidates = append(candidates, item)
			}
		}
	}
	slices.SortFunc(candidates, func(a, b *media.Item) int {
		if c := snapshot.User.Items[b.ID].LastPlayed.Compare(snapshot.User.Items[a.ID].LastPlayed); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	result := []object{}
	for _, item := range candidates[:min(limit, len(candidates))] {
		result = append(result, s.movieDTO(*item, serverID, snapshot.User.Items, token))
	}
	return result
}

func (s *Server) playbackRoutes(mux *http.ServeMux) {
	for path, event := range map[string]string{
		"/sessions/playing":          "start",
		"/sessions/playing/progress": "progress",
		"/sessions/playing/stopped":  "stop",
	} {
		mux.HandleFunc("POST "+path, s.protect(s.reportPlaystate(event)))
	}
	for _, path := range []string{"/users/{user}/playeditems/{item}", "/users/{user}/favoriteitems/{item}"} {
		favorite := strings.Contains(path, "favorite")
		mux.HandleFunc("POST "+path, s.protect(s.setItemFlag(favorite, true)))
		mux.HandleFunc("DELETE "+path, s.protect(s.setItemFlag(favorite, false)))
	}
	mux.HandleFunc("GET /users/{user}/items/resume", s.protect(func(w http.ResponseWriter, r *http.Request, token string, session state.Session) {
		s.listItems(w, r, false, true)
	}))
	mux.HandleFunc("POST /items/{item}/playbackinfo", s.protect(s.playbackInfo))
	// Emby also answers GET for clients that cannot post a profile.
	mux.HandleFunc("GET /items/{item}/playbackinfo", s.protect(s.playbackInfo))
	for _, prefix := range []string{"/videos", "/audio"} {
		mux.HandleFunc("GET "+prefix+"/{item}/{stream}", s.protect(s.streamVideo))
		mux.HandleFunc("HEAD "+prefix+"/{item}/{stream}", s.protect(s.streamVideo))
	}
}

// streamRoute rejects anything but the direct stream endpoint. Coach does not
// transcode, so the HLS and segment routes are deliberately not served.
func streamRoute(name string) bool {
	return strings.TrimSuffix(name, path.Ext(name)) == "stream"
}
