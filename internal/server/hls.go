package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/TokenCemetery/coach/internal/hls"
	"github.com/TokenCemetery/coach/internal/media"
	"github.com/TokenCemetery/coach/internal/state"
)

// SetHLS enables remux to HLS. Without it Coach offers only Direct Stream.
// The caller owns the manager and closes it after the HTTP server.
func (s *Server) SetHLS(m *hls.Manager) {
	s.hls = m
}

type transcodingProfile struct {
	Type, Container, Protocol, VideoCodec, AudioCodec string
}

// remuxCodecs returns the codecs of an HLS remux the profile accepts: the
// video is copied, and the audio is copied when its codec is accepted and
// encoded to AAC otherwise. Video transcoding is not implemented (#22), so a
// video codec, profile or bitrate the client rejects rules remux out.
func (p playbackRequest) remuxCodecs(item media.Item, video, audio *media.Stream) (videoCodec, audioCodec string, channels int, ok bool) {
	if video == nil || p.DeviceProfile == nil || !p.withinBitrate(item) {
		return "", "", 0, false
	}
	for _, tp := range p.DeviceProfile.TranscodingProfiles {
		if !strings.EqualFold(tp.Type, "Video") || !strings.EqualFold(tp.Protocol, "hls") || !strings.EqualFold(tp.Container, "ts") || !member(tp.VideoCodec, video.Codec) {
			continue
		}
		segmented := item
		segmented.Container = "ts"
		if audio == nil {
			return video.Codec, "", 0, codecProfilesMatch(p.DeviceProfile, segmented, video, nil)
		}
		if member(tp.AudioCodec, audio.Codec) && p.audioChannelsAllowed(audio) && codecProfilesMatch(p.DeviceProfile, segmented, video, audio) {
			return video.Codec, audio.Codec, 0, true
		}
		if member(tp.AudioCodec, "aac") && codecProfilesMatch(p.DeviceProfile, segmented, video, nil) {
			return video.Codec, "aac", encodedChannels(audio.Channels, p.MaxAudioChannels), true
		}
	}
	return "", "", 0, false
}

// encodedChannels keeps the source layout up to the client's limit, or
// stereo when the client sets none.
func encodedChannels(source int, limit *int64) int {
	most := 2
	if limit != nil && *limit > 0 {
		most = int(min(*limit, 8))
	}
	if source <= 0 {
		return most
	}
	return min(source, most)
}

func transcodingURL(item media.Item, deviceID, playSession, token, videoCodec, audioCodec string, audio *media.Stream, channels int) string {
	query := url.Values{}
	query.Set("MediaSourceId", "mediasource_"+item.ID)
	query.Set("PlaySessionId", playSession)
	query.Set("api_key", token)
	query.Set("VideoCodec", videoCodec)
	if audio != nil {
		query.Set("AudioCodec", audioCodec)
		query.Set("AudioStreamIndex", strconv.Itoa(audio.Index))
	}
	if channels > 0 {
		query.Set("MaxAudioChannels", strconv.Itoa(channels))
	}
	if deviceID != "" {
		query.Set("DeviceId", deviceID)
	}
	return "/videos/" + item.ID + "/master.m3u8?" + query.Encode()
}

// remuxSource is the video-only remux of item; callers set the audio.
func remuxSource(catalog *media.Catalog, item media.Item, video *media.Stream) hls.Source {
	return hls.Source{ItemID: item.ID, Version: fmt.Sprintf("%d/%d", item.Size, item.Modified.UnixNano()),
		Open: func() (*os.File, error) { return catalog.Open(item) }, Duration: float64(item.RunTimeTicks) / 1e7,
		VideoStream: video.Index, AudioStream: -1}
}

// hlsSource derives the remux from an HLS request's query, so that the
// master playlist, media playlist and segments each work on their own. Emby
// Web sends PlaySessionId and the codecs; for a client that omits them the
// session is keyed by its token and stream, the video is copied and the audio
// is copied when it is AAC or MP3. On failure the response has been written.
func (s *Server) hlsSource(w http.ResponseWriter, r *http.Request, session state.Session) (playSession string, source hls.Source, ok bool) {
	if s.hls == nil {
		fail(w, 404, "NotFound")
		return "", source, false
	}
	item, found := s.findItem(r.PathValue("item"))
	if !found || item.IsFolder() {
		fail(w, 404, "NotFound")
		return "", source, false
	}
	query := r.URL.Query()
	playSession = query.Get("PlaySessionId")
	if len(playSession) > 128 {
		fail(w, 400, "InvalidRequest")
		return "", source, false
	}
	if id := query.Get("MediaSourceId"); id != "" && id != "mediasource_"+item.ID {
		fail(w, 404, "MediaSourceNotFound")
		return "", source, false
	}
	var audioIndex *int64
	if text := query.Get("AudioStreamIndex"); text != "" {
		index, err := strconv.ParseInt(text, 10, 32)
		if err != nil || index < 0 {
			fail(w, 400, "InvalidRequest")
			return "", source, false
		}
		audioIndex = &index
	}
	video, audio := playbackStreams(item, audioIndex)
	if video == nil || (audioIndex != nil && audio == nil) {
		fail(w, 400, "InvalidRequest")
		return "", source, false
	}
	if codecs := query.Get("VideoCodec"); codecs != "" && !member(codecs, video.Codec) {
		// Copy is the only video path: transcoding is not implemented (#22).
		failText(w, 400, "Coach can only remux this video, and the requested codecs exclude its "+video.Codec+" video.")
		return "", source, false
	}
	source = remuxSource(s.catalog(), item, video)
	if audio != nil {
		source.AudioStream = audio.Index
		codecs := query.Get("AudioCodec")
		if codecs == "" {
			codecs = "aac,mp3"
		}
		switch {
		case member(codecs, audio.Codec):
		case member(codecs, "aac"):
			var limit *int64
			if text := query.Get("MaxAudioChannels"); text != "" {
				n, err := strconv.ParseInt(text, 10, 32)
				if err != nil || n < 0 {
					fail(w, 400, "InvalidRequest")
					return "", source, false
				}
				limit = &n
			}
			source.AudioEncode, source.AudioChannels = true, encodedChannels(audio.Channels, limit)
		default:
			fail(w, 400, "InvalidRequest")
			return "", source, false
		}
	}
	if playSession == "" {
		sum := sha256.Sum256([]byte(session.ID + "\x00" + source.Key()))
		playSession = "auto-" + hex.EncodeToString(sum[:8])
	}
	return playSession, source, true
}

// hlsFailure answers a manager error.
func hlsFailure(w http.ResponseWriter, r *http.Request, item string, err error) {
	switch {
	case r.Context().Err() != nil:
	case errors.Is(err, hls.ErrSuperseded):
		w.Header().Set("Retry-After", "1")
		fail(w, 503, "TooManyRequests")
	case errors.Is(err, hls.ErrBusy):
		w.Header().Set("Retry-After", "10")
		fail(w, 503, "TooManyRequests")
	case errors.Is(err, hls.ErrConflict):
		fail(w, 409, "Conflict")
	case errors.Is(err, hls.ErrNotFound):
		fail(w, 404, "NotFound")
	default:
		slog.Warn("HLS stream unavailable", "item", item)
		fail(w, 500, "MediaUnavailable")
	}
}

// masterPlaylist names one variant: the media playlist with the same query.
func (s *Server) masterPlaylist(w http.ResponseWriter, r *http.Request, token string, session state.Session) {
	_, source, ok := s.hlsSource(w, r, session)
	if !ok {
		return
	}
	item, _ := s.findItem(source.ItemID)
	bandwidth := bitrate(item)
	if bandwidth <= 0 {
		bandwidth = 1_000_000
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	// The query is re-encoded, so no raw request text reaches the playlist.
	_, _ = fmt.Fprintf(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=%d\nmain.m3u8?%s\n", bandwidth, r.URL.Query().Encode()) //nolint:gosec // an HLS playlist, not HTML, with an encoded query
}

func (s *Server) mediaPlaylist(w http.ResponseWriter, r *http.Request, token string, session state.Session) {
	playSession, source, ok := s.hlsSource(w, r, session)
	if !ok {
		return
	}
	query := r.URL.Query().Encode()
	playlist, err := s.hls.Playlist(r.Context(), playSession, session.ID, source, func(n int) string {
		return "hls1/main/" + strconv.Itoa(n) + ".ts?" + query
	})
	if err != nil {
		hlsFailure(w, r, source.ItemID, err)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(playlist)) //nolint:gosec // an HLS playlist, not HTML, with an encoded query
}

func (s *Server) hlsSegment(w http.ResponseWriter, r *http.Request, token string, session state.Session) {
	name, found := strings.CutSuffix(r.PathValue("segment"), ".ts")
	n, err := strconv.Atoi(name)
	if !found || err != nil || n < 0 {
		fail(w, 404, "NotFound")
		return
	}
	playSession, source, ok := s.hlsSource(w, r, session)
	if !ok {
		return
	}
	file, err := s.hls.Segment(r.Context(), playSession, session.ID, source, n)
	if err != nil {
		hlsFailure(w, r, source.ItemID, err)
		return
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		s.internalError(w)
		return
	}
	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, "", info.ModTime(), file)
}

func (s *Server) hlsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /videos/{item}/master.m3u8", s.protect(s.masterPlaylist))
	mux.HandleFunc("GET /videos/{item}/main.m3u8", s.protect(s.mediaPlaylist))
	mux.HandleFunc("GET /videos/{item}/hls1/{playlist}/{segment}", s.protect(s.hlsSegment))
	mux.HandleFunc("DELETE /videos/activeencodings", s.protect(s.stopEncodings))
	mux.HandleFunc("POST /videos/activeencodings/delete", s.protect(s.stopEncodings))
}

// stopEncodings ends the caller's HLS session for PlaySessionId, or all of
// the caller's sessions without one. Emby Web sends it when playback of a
// transcoded stream stops.
func (s *Server) stopEncodings(w http.ResponseWriter, r *http.Request, _ string, session state.Session) {
	playSession := value(r, "PlaySessionId")
	if len(playSession) > 128 {
		fail(w, 400, "InvalidRequest")
		return
	}
	if s.hls != nil {
		if playSession == "" {
			s.hls.StopOwner(session.ID)
		} else {
			s.hls.Stop(playSession, session.ID)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
