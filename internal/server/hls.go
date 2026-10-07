package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
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
// encoded to AAC otherwise. A video codec, profile or bitrate the client
// rejects rules remux out; transcodeCodecs covers that case.
func (p playbackRequest) remuxCodecs(item media.Item, video, audio *media.Stream) (videoCodec, audioCodec string, channels int, ok bool) {
	if video == nil || p.DeviceProfile == nil || !p.withinBitrate(item) {
		return "", "", 0, false
	}
	for _, tp := range p.DeviceProfile.TranscodingProfiles {
		if !strings.EqualFold(tp.Type, "Video") || !strings.EqualFold(tp.Protocol, "hls") || !strings.EqualFold(tp.Container, "ts") || !member(tp.VideoCodec, video.Codec) {
			continue
		}
		// The segments hold only the video and the selected audio, so that
		// track is the primary one there (IsSecondaryAudio).
		segmented := item
		segmented.Container = "ts"
		segmented.Streams = []media.Stream{*video}
		if audio != nil {
			segmented.Streams = append(segmented.Streams, *audio)
		}
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

// videoEncoding limits a video encoded to H.264. Bitrate is positive for an
// encoded video and zero for a copied one; a zero size or level is
// unlimited. Level is the profile's VideoLevel, such as 42 for 4.2.
type videoEncoding struct {
	Bitrate             int64
	MaxWidth, MaxHeight int
	Level               int
}

// transcodeCodecs returns the audio codec of an HLS stream whose video is
// encoded to H.264, when the profile accepts H.264 in HLS, and the limits of
// the encoded video. The audio is copied or encoded to AAC as in remuxCodecs.
func (p playbackRequest) transcodeCodecs(item media.Item, video, audio *media.Stream) (audioCodec string, channels int, encoding videoEncoding, ok bool) {
	if video == nil || p.DeviceProfile == nil {
		return "", 0, encoding, false
	}
	encoding = p.h264Limits()
	encoding.fitLevel(video.Width, video.Height)
	if bitrate := p.transcodeBitrate(item); bitrate < 0 {
		return "", 0, encoding, false
	} else if encoding.Bitrate == 0 || bitrate < encoding.Bitrate {
		encoding.Bitrate = bitrate
	}
	for _, tp := range p.DeviceProfile.TranscodingProfiles {
		if !strings.EqualFold(tp.Type, "Video") || !strings.EqualFold(tp.Protocol, "hls") || !strings.EqualFold(tp.Container, "ts") || !member(tp.VideoCodec, "h264") {
			continue
		}
		switch {
		case audio == nil:
			return "", 0, encoding, true
		case member(tp.AudioCodec, audio.Codec) && p.audioChannelsAllowed(audio):
			return audio.Codec, 0, encoding, true
		case member(tp.AudioCodec, "aac"):
			return "aac", encodedChannels(audio.Channels, p.MaxAudioChannels), encoding, true
		}
	}
	return "", 0, encoding, false
}

// h264Limits collects the LessThanEqual limits on Width, Height, VideoLevel and
// VideoBitrate from the profile's video codec conditions that apply to H.264
// in MPEG-TS, such as Emby Web's Width 1920 for a browser that cannot decode
// 4K smoothly. Conditions on the source (ApplyConditions) and other
// properties are not evaluated for the encoded video.
func (p playbackRequest) h264Limits() videoEncoding {
	var limits videoEncoding
	lower := func(current, limit int64) int64 {
		if current == 0 || limit < current {
			return limit
		}
		return current
	}
	for _, cp := range p.DeviceProfile.CodecProfiles {
		if !strings.EqualFold(cp.Type, "Video") || len(cp.ApplyConditions) > 0 ||
			(cp.Codec != "" && !member(cp.Codec, "h264")) || (cp.Container != "" && !member(cp.Container, "ts")) {
			continue
		}
		for _, c := range cp.Conditions {
			value, ok := conditionNumber(string(c.Value))
			if !strings.EqualFold(c.Condition, "LessThanEqual") || !ok || value < 1 || value > math.MaxInt32 {
				continue
			}
			switch strings.ToLower(c.Property) {
			case "width":
				limits.MaxWidth = int(lower(int64(limits.MaxWidth), int64(value)))
			case "height":
				limits.MaxHeight = int(lower(int64(limits.MaxHeight), int64(value)))
			case "videobitrate":
				limits.Bitrate = lower(limits.Bitrate, int64(value))
			case "videolevel":
				limits.Level = int(lower(int64(limits.Level), int64(value)))
			}
		}
	}
	return limits
}

// h264FrameSizes maps H.264 levels (as Emby writes them, 42 for 4.2) to the
// largest frame they allow, in 16x16 macroblocks (MaxFS, ITU-T H.264 Table
// A-1).
var h264FrameSizes = []struct{ level, macroblocks int }{
	{10, 99}, {11, 396}, {21, 792}, {22, 1620}, {31, 3600}, {32, 5120},
	{40, 8192}, {42, 8704}, {50, 22080}, {51, 36864}, {60, 139264},
}

// fitLevel lowers MaxWidth and MaxHeight so that a source of width x height,
// scaled to the current limits with its aspect ratio kept, also fits the
// largest frame of the level. The level's macroblock rate, which bounds the
// frame rate, is not checked. An unknown size or level changes nothing.
func (e *videoEncoding) fitLevel(width, height int) {
	if e.Level <= 0 || width <= 0 || height <= 0 {
		return
	}
	most := 0
	for _, l := range h264FrameSizes {
		if l.level <= e.Level {
			most = l.macroblocks
		}
	}
	if most == 0 {
		return
	}
	scale := 1.0
	if e.MaxWidth > 0 {
		scale = min(scale, float64(e.MaxWidth)/float64(width))
	}
	if e.MaxHeight > 0 {
		scale = min(scale, float64(e.MaxHeight)/float64(height))
	}
	macroblocks := func(s float64) int {
		w, h := int(float64(width)*s)/2*2, int(float64(height)*s)/2*2
		return ((w + 15) / 16) * ((h + 15) / 16)
	}
	if macroblocks(scale) <= most {
		return
	}
	scale = math.Sqrt(float64(most*256) / float64(width*height))
	for scale > 0 && macroblocks(scale) > most {
		scale -= 0.001
	}
	e.MaxWidth, e.MaxHeight = max(int(float64(width)*scale)/2*2, 2), max(int(float64(height)*scale)/2*2, 2)
}

// Without a client limit, an encoded video of unknown bitrate is capped at
// defaultVideoBitrate; audioAllowance is left from a client limit for audio,
// and the cap is never below minVideoBitrate.
const (
	defaultVideoBitrate = 8_000_000
	audioAllowance      = 192_000
	minVideoBitrate     = 100_000
)

// transcodeBitrate caps the encoded video at the source's bitrate and the
// client's limit less room for audio, or returns -1 for a negative limit.
// The result is positive otherwise, so VideoBitrate marks an encoded stream.
func (p playbackRequest) transcodeBitrate(item media.Item) int64 {
	rate := bitrate(item)
	if rate <= 0 {
		rate = defaultVideoBitrate
	}
	limits := []*int64{p.MaxStreamingBitrate}
	if p.DeviceProfile != nil {
		limits = append(limits, p.DeviceProfile.MaxStreamingBitrate)
	}
	for _, limit := range limits {
		switch {
		case limit == nil || *limit == 0:
		case *limit < 0:
			return -1
		default:
			rate = min(rate, max(*limit-audioAllowance, *limit/2))
		}
	}
	return max(rate, minVideoBitrate)
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

// transcodingURL names the HLS stream; VideoBitrate in it means the video is
// encoded to videoCodec within the encoding limits rather than copied.
func transcodingURL(item media.Item, deviceID, playSession, token, videoCodec, audioCodec string, audio *media.Stream, channels int, encoding videoEncoding) string {
	query := url.Values{}
	query.Set("MediaSourceId", "mediasource_"+item.ID)
	query.Set("PlaySessionId", playSession)
	query.Set("api_key", token)
	query.Set("VideoCodec", videoCodec)
	if encoding.Bitrate > 0 {
		query.Set("VideoBitrate", strconv.FormatInt(encoding.Bitrate, 10))
		if encoding.MaxWidth > 0 {
			query.Set("MaxWidth", strconv.Itoa(encoding.MaxWidth))
		}
		if encoding.MaxHeight > 0 {
			query.Set("MaxHeight", strconv.Itoa(encoding.MaxHeight))
		}
	}
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

// hlsSource derives the stream from an HLS request's query, so that the
// master playlist, media playlist and segments each work on their own. Emby
// Web sends PlaySessionId and the codecs; for a client that omits them the
// session is keyed by its token and stream, the video is copied and the audio
// is copied when it is AAC or MP3. The video is encoded to H.264 when the
// query sets VideoBitrate or its codecs exclude the source's but include
// H.264. On failure the response has been written.
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
	source = remuxSource(s.catalog(), item, video)
	if text := query.Get("VideoBitrate"); text != "" {
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil || n <= 0 {
			fail(w, 400, "InvalidRequest")
			return "", source, false
		}
		source.VideoEncode, source.VideoBitrate = true, n
	}
	for name, size := range map[string]*int{"MaxWidth": &source.MaxWidth, "MaxHeight": &source.MaxHeight} {
		if text := query.Get(name); text != "" {
			n, err := strconv.ParseInt(text, 10, 32)
			if err != nil || n < 2 {
				fail(w, 400, "InvalidRequest")
				return "", source, false
			}
			*size = int(n)
		}
	}
	if codecs := query.Get("VideoCodec"); codecs != "" && !member(codecs, video.Codec) {
		if !member(codecs, "h264") {
			failText(w, 400, "Coach encodes video only to H.264, and the requested codecs exclude it and the source's "+video.Codec+".")
			return "", source, false
		}
		source.VideoEncode = true
	}
	if source.VideoEncode && source.VideoBitrate == 0 {
		source.VideoBitrate = playbackRequest{}.transcodeBitrate(item)
	}
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
	if source.VideoEncode {
		bandwidth = source.VideoBitrate + audioAllowance
	}
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
