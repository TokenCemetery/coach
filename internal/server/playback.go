package server

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"math"
	"mime"
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
	defer func() { _ = file.Close() }()
	serveMediaFile(w, r, file, item)
}

// downloadItem serves the original file as an attachment for the "Download"
// command. Emby Web navigates to the URL, so the token arrives as api_key.
// The file name is built from the item name: Path is never sent to clients.
func (s *Server) downloadItem(w http.ResponseWriter, r *http.Request, token string, session state.Session) {
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
	defer func() { _ = file.Close() }()
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": item.Name + path.Ext(item.Path)}))
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
		problem := ""
		_, err := s.updateItemSession(token, item, func(st *state.ItemState, current *state.Session) {
			if problem = playReportProblem(current, item.ID, body.PlaySessionId, event); problem != "" {
				return
			}
			if hasPosition {
				st.PositionTicks = position
			}
			st.LastPlayed = time.Now().UTC()
			st.HiddenFromResume = false
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
		// The client is still answered 204: a duplicate or late report is
		// normal after retries. The log lets a lost position be diagnosed.
		if err == nil && problem != "" {
			slog.Warn("Playback report ignored", "event", event, "reason", problem, "item", item.ID)
		}
		if err == nil && problem == "" && event == "stop" && !hasPosition {
			slog.Info("Playback stopped without a position", "item", item.ID)
		}
		s.changed(w, err)
	}
}

// ID-bearing reports use a durable 64-start retry window per client session.
// Only the newest start accepts progress/stop; a completed play cannot reopen.
// Legacy reports without an ID retain arrival-order semantics.
func acceptPlayReport(session *state.Session, itemID, playID, event string) bool {
	return playReportProblem(session, itemID, playID, event) == ""
}

// playReportProblem applies acceptPlayReport's rules and names the rule that
// rejects a report, or returns "" when the report is accepted.
func playReportProblem(session *state.Session, itemID, playID, event string) string {
	if playID == "" {
		return ""
	}
	index := slices.IndexFunc(session.RecentPlays, func(p state.PlaybackRecord) bool { return p.ID == playID })
	if event == "start" {
		if index >= 0 {
			return "repeated start"
		}
		if len(session.RecentPlays) >= 64 {
			session.RecentPlays = slices.Clone(session.RecentPlays[len(session.RecentPlays)-63:])
		}
		session.RecentPlays = append(session.RecentPlays, state.PlaybackRecord{ID: playID, ItemID: itemID})
		return ""
	}
	switch {
	case index < 0:
		return "unknown play session"
	case index != len(session.RecentPlays)-1:
		return "not the newest play session"
	}
	play := &session.RecentPlays[index]
	switch {
	case play.Stopped:
		return "play session already stopped"
	case play.ItemID != itemID:
		return "play session belongs to another item"
	}
	if event == "stop" {
		play.Stopped = true
	}
	return ""
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
		// Marking a series or season played marks its episodes, as in Emby; the
		// folder's own Played is derived from them.
		if !favorite && item.IsFolder() {
			for _, episode := range s.folderEpisodes(item) {
				if _, err := s.updateItem(token, episode, func(st *state.ItemState) { setPlayed(st, value) }); err != nil {
					s.changed(w, err)
					return
				}
			}
			data := s.folderPlayedData(item, s.store.Snapshot().User.Items)
			data["ItemId"] = item.ID
			respond(w, 200, data)
			return
		}
		data, err := s.updateItem(token, item, func(st *state.ItemState) {
			if favorite {
				st.IsFavorite = value
				return
			}
			setPlayed(st, value)
		})
		if err != nil {
			s.changed(w, err)
			return
		}
		respond(w, 200, data)
	}
}

func setPlayed(st *state.ItemState, value bool) {
	st.Played = value
	if value {
		st.PositionTicks = 0
		st.LastPlayed = time.Now().UTC()
		if st.PlayCount == 0 {
			st.PlayCount = 1
		}
	}
}

// hideFromResume serves "Remove from Continue Watching". Hiding clears the
// stored position, which removes an item in progress, and marks the item so a
// next-up episode is not offered either. Hide=false clears the mark only; the
// position is not restored.
func (s *Server) hideFromResume(w http.ResponseWriter, r *http.Request, token string, session state.Session) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	var hide *bool
	if err != nil || playbackQuery(query, "Hide", &hide, strconv.ParseBool) != nil || hide == nil {
		fail(w, 400, "InvalidRequest")
		return
	}
	item, found := s.findItem(r.PathValue("item"))
	if !found {
		fail(w, 404, "NotFound")
		return
	}
	data, err := s.updateItem(token, item, func(st *state.ItemState) {
		if *hide {
			st.PositionTicks = 0
		}
		st.HiddenFromResume = *hide
	})
	if err != nil {
		s.changed(w, err)
		return
	}
	respond(w, 200, data)
}

// resumeItems fills the home Continue Watching row: items with meaningful
// stored progress plus, because the section advertises IncludeNextUpInResume,
// each started series' next episode dated by the series' last watch. Most
// recent first.
func (s *Server) resumeItems(limit int, token string) []object {
	snapshot := s.store.Snapshot()
	serverID := snapshot.ServerID
	candidates := []*media.Item{}
	played := map[string]time.Time{}
	for _, c := range s.nextUpEpisodes(snapshot.User.Items, "", "") {
		if snapshot.User.Items[c.next.ID].HiddenFromResume {
			continue
		}
		candidates = append(candidates, c.next)
		played[c.next.ID] = c.lastPlayed
	}
	if s.media != nil {
		for i := range s.media.Items {
			item := &s.media.Items[i]
			st := snapshot.User.Items[item.ID]
			if !resumable(st, item.RunTimeTicks) {
				continue
			}
			if _, listed := played[item.ID]; !listed {
				candidates = append(candidates, item)
			}
			if st.LastPlayed.After(played[item.ID]) {
				played[item.ID] = st.LastPlayed
			}
		}
	}
	slices.SortFunc(candidates, func(a, b *media.Item) int {
		if c := played[b.ID].Compare(played[a.ID]); c != 0 {
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
		// Emby Web clears the flag with POST .../Delete instead of DELETE.
		mux.HandleFunc("POST "+path+"/delete", s.protect(s.setItemFlag(favorite, false)))
	}
	mux.HandleFunc("POST /users/{user}/items/{item}/hidefromresume", s.protect(s.hideFromResume))
	mux.HandleFunc("GET /users/{user}/items/resume", s.protect(func(w http.ResponseWriter, r *http.Request, token string, session state.Session) {
		s.listItems(w, r, false, true)
	}))
	mux.HandleFunc("POST /items/{item}/playbackinfo", s.protect(s.playbackInfo))
	// Emby also answers GET for clients that cannot post a profile.
	mux.HandleFunc("GET /items/{item}/playbackinfo", s.protect(s.playbackInfo))
	mux.HandleFunc("GET /items/{item}/download", s.protect(s.downloadItem))
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
