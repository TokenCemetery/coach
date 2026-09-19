package server

import (
	"errors"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/TokenCemetery/coach/internal/media"
)

type playbackRequest struct {
	DeviceProfile       *deviceProfile
	UserId              *string
	MediaSourceId       *string
	EnableDirectStream  *bool
	MaxStreamingBitrate *int64
	MaxAudioChannels    *int64
	StartTimeTicks      *int64
	AudioStreamIndex    *int64
	SubtitleStreamIndex *int64
}

// Emby Web puts the profile in JSON and playback options in the query string.
// Accept either location, but reject conflicting values instead of guessing.
func readPlaybackRequest(w http.ResponseWriter, r *http.Request) (playbackRequest, error) {
	var body playbackRequest
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return body, errors.New("invalid playback query")
	}
	if r.ContentLength != 0 {
		if err := decodeJSON(w, r, &body); err != nil {
			return body, err
		}
	}
	integer := func(s string) (int64, error) { return strconv.ParseInt(s, 10, 64) }
	text := func(s string) (string, error) { return s, nil }
	for _, err := range []error{
		playbackQuery(query, "UserId", &body.UserId, text),
		playbackQuery(query, "MediaSourceId", &body.MediaSourceId, text),
		playbackQuery(query, "EnableDirectStream", &body.EnableDirectStream, strconv.ParseBool),
		playbackQuery(query, "MaxStreamingBitrate", &body.MaxStreamingBitrate, integer),
		playbackQuery(query, "MaxAudioChannels", &body.MaxAudioChannels, integer),
		playbackQuery(query, "StartTimeTicks", &body.StartTimeTicks, integer),
		playbackQuery(query, "AudioStreamIndex", &body.AudioStreamIndex, integer),
		playbackQuery(query, "SubtitleStreamIndex", &body.SubtitleStreamIndex, integer),
	} {
		if err != nil {
			return body, err
		}
	}
	for _, p := range []*int64{body.MaxStreamingBitrate, body.MaxAudioChannels, body.StartTimeTicks, body.AudioStreamIndex} {
		if p != nil && *p < 0 {
			return body, errors.New("negative playback parameter")
		}
	}
	if body.SubtitleStreamIndex != nil && *body.SubtitleStreamIndex < -1 {
		return body, errors.New("invalid subtitle index")
	}
	for _, p := range []*int64{body.AudioStreamIndex, body.SubtitleStreamIndex, body.MaxAudioChannels} {
		if p != nil && *p > math.MaxInt32 {
			return body, errors.New("playback parameter exceeds int32")
		}
	}
	return body, nil
}

func playbackQuery[T comparable](query url.Values, key string, dst **T, parse func(string) (T, error)) error {
	for name, values := range query {
		if !strings.EqualFold(name, key) {
			continue
		}
		for _, raw := range values {
			v, err := parse(raw)
			if err != nil || (*dst != nil && **dst != v) {
				return errors.New("invalid or conflicting playback parameter")
			}
			*dst = &v
		}
	}
	return nil
}

func playbackStreams(item media.Item, audioIndex *int64) (video, audio *media.Stream) {
	index, hasAudio := defaultAudioStreamIndex(item)
	if audioIndex != nil {
		index, hasAudio = int(*audioIndex), true
	}
	for i := range item.Streams {
		stream := &item.Streams[i]
		if stream.Type == "Video" && video == nil {
			video = stream
		}
		if hasAudio && stream.Type == "Audio" && stream.Index == index {
			audio = stream
		}
	}
	return video, audio
}

func hasSubtitle(item media.Item, index int64) bool {
	for _, stream := range item.Streams {
		if stream.Type == "Subtitle" && int64(stream.Index) == index {
			return true
		}
	}
	return false
}

func (p playbackRequest) supportsDirectStream(item media.Item, video, audio *media.Stream) bool {
	if video == nil || (p.EnableDirectStream != nil && !*p.EnableDirectStream) {
		return false
	}
	// Subtitle delivery/burn-in is not implemented. Never promise a selected
	// subtitle that the server cannot deliver.
	if p.SubtitleStreamIndex != nil && *p.SubtitleStreamIndex >= 0 {
		return false
	}
	if p.MaxAudioChannels != nil && *p.MaxAudioChannels > 0 && audio != nil && (audio.Channels <= 0 || int64(audio.Channels) > *p.MaxAudioChannels) {
		return false
	}
	limits := []*int64{p.MaxStreamingBitrate}
	if p.DeviceProfile != nil {
		limits = append(limits, p.DeviceProfile.MaxStreamingBitrate)
	}
	for _, limit := range limits {
		if limit != nil && (*limit < 0 || (*limit > 0 && (bitrate(item) <= 0 || bitrate(item) > *limit))) {
			return false
		}
	}
	return supportsProfile(p.DeviceProfile, item, video, audio)
}
