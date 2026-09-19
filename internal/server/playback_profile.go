package server

import (
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"

	"github.com/TokenCemetery/coach/internal/media"
)

type directPlayProfile struct {
	Container, Type, VideoCodec, AudioCodec string
}

type deviceProfile struct {
	MaxStreamingBitrate *int64
	DirectPlayProfiles  []directPlayProfile
	CodecProfiles       []codecProfile
	ContainerProfiles   []codecProfile
}

type codecProfile struct {
	Type, Codec, Container string
	Conditions             []profileCondition
	ApplyConditions        []profileCondition
}

type profileCondition struct {
	Condition, Property string
	Value               conditionValue
	IsRequired          *conditionBool
}

type conditionBool bool

func (b *conditionBool) UnmarshalJSON(raw []byte) error {
	// Emby Web 4.10 sends IsRequired as both a boolean and a quoted boolean.
	v := strings.Trim(string(raw), `"`)
	if v != "true" && v != "false" {
		return errors.New("invalid condition boolean")
	}
	*b = conditionBool(v == "true")
	return nil
}

type conditionValue string

func (v *conditionValue) UnmarshalJSON(raw []byte) error {
	var text string
	if len(raw) > 0 && raw[0] == '"' {
		if err := json.Unmarshal(raw, &text); err != nil {
			return err
		}
	} else {
		// Numeric values occur in the web client's VideoRotation condition.
		text = string(raw)
		if text != "true" && text != "false" {
			if _, ok := conditionNumber(text); !ok {
				return errors.New("invalid condition value")
			}
		}
	}
	*v = conditionValue(text)
	return nil
}

func supportsProfile(profile *deviceProfile, item media.Item, video, audio *media.Stream) bool {
	// No profile preserves discovery/legacy requests. An explicit empty profile
	// declares no playable formats and must not be treated as unrestricted.
	if profile == nil {
		return true
	}
	matched := false
	for _, p := range profile.DirectPlayProfiles {
		if (p.Type == "" || strings.EqualFold(p.Type, "Video")) &&
			(p.Container == "" || member(p.Container, item.Container)) &&
			(p.VideoCodec == "" || member(p.VideoCodec, video.Codec)) &&
			(audio == nil || p.AudioCodec == "" || member(p.AudioCodec, audio.Codec)) {
			matched = true
			break
		}
	}
	if !matched {
		return false
	}
	for _, p := range profile.ContainerProfiles {
		if strings.EqualFold(p.Type, "Video") && (p.Container == "" || member(p.Container, item.Container)) &&
			!conditionsMatch(p.Conditions, item, video, audio) {
			return false
		}
	}
	for _, p := range profile.CodecProfiles {
		stream := video
		switch strings.ToLower(p.Type) {
		case "video":
		case "videoaudio":
			stream = audio
		default:
			continue
		}
		if stream == nil || (p.Codec != "" && !member(p.Codec, stream.Codec)) ||
			(p.Container != "" && !member(p.Container, item.Container)) {
			continue
		}
		if conditionsMatch(p.ApplyConditions, item, video, audio) && !conditionsMatch(p.Conditions, item, video, audio) {
			return false
		}
	}
	return true
}

func conditionsMatch(conditions []profileCondition, item media.Item, video, audio *media.Stream) bool {
	for _, c := range conditions {
		if !conditionMatches(c, conditionProperty(c.Property, item, video, audio)) {
			return false
		}
	}
	return true
}

func conditionNumber(s string) (float64, bool) {
	n, err := strconv.ParseFloat(s, 64)
	return n, err == nil && !math.IsNaN(n) && !math.IsInf(n, 0)
}

func conditionMatches(c profileCondition, actual any) bool {
	op := strings.ToLower(c.Condition)
	switch op {
	case "equals", "notequals", "equalsany", "lessthanequal", "greaterthanequal":
	default:
		return false
	}
	if actual == nil {
		return c.IsRequired != nil && !bool(*c.IsRequired)
	}
	values := []string{string(c.Value)}
	if op == "equalsany" {
		values = strings.Split(string(c.Value), "|")
	}
	for _, value := range values {
		value = strings.TrimSpace(value)
		var equal bool
		switch got := actual.(type) {
		case float64:
			want, ok := conditionNumber(value)
			if !ok {
				continue
			}
			if op == "lessthanequal" {
				return got <= want
			}
			if op == "greaterthanequal" {
				return got >= want
			}
			equal = got == want
		case string:
			equal = strings.EqualFold(got, value)
		case bool:
			if !strings.EqualFold(value, "true") && !strings.EqualFold(value, "false") {
				continue
			}
			equal = got == strings.EqualFold(value, "true")
		}
		if op == "notequals" {
			return !equal
		}
		if (op == "equals" || op == "equalsany") && equal {
			return true
		}
	}
	return false
}

func conditionProperty(name string, item media.Item, video, audio *media.Stream) any {
	positive := func(n float64) any {
		if n > 0 && !math.IsNaN(n) && !math.IsInf(n, 0) {
			return n
		}
		return nil
	}
	text := func(s string) any {
		if s != "" {
			return s
		}
		return nil
	}
	switch strings.ToLower(name) {
	case "width":
		return positive(float64(video.Width))
	case "height":
		return positive(float64(video.Height))
	case "videolevel":
		return positive(video.Level)
	case "videoprofile":
		return text(video.Profile)
	case "videocodectag":
		return text(video.CodecTag)
	case "videobitrate":
		return positive(float64(video.BitRate))
	case "videobitdepth":
		return positive(float64(video.BitDepth))
	case "videoframerate":
		if video.AverageFrameRate > 0 {
			return positive(video.AverageFrameRate)
		}
		return positive(video.RealFrameRate)
	case "refframes":
		return positive(float64(video.RefFrames))
	case "isinterlaced":
		return video.IsInterlaced
	case "numaudiostreams", "numvideostreams":
		kind := "Audio"
		if strings.EqualFold(name, "NumVideoStreams") {
			kind = "Video"
		}
		count := 0
		for _, stream := range item.Streams {
			if stream.Type == kind {
				count++
			}
		}
		return float64(count)
	}
	if audio != nil {
		switch strings.ToLower(name) {
		case "audiochannels":
			return positive(float64(audio.Channels))
		case "audiobitrate":
			return positive(float64(audio.BitRate))
		case "audioprofile":
			return text(audio.Profile)
		case "audiosamplerate", "samplerate":
			return positive(float64(audio.SampleRate))
		case "issecondaryaudio":
			for _, stream := range item.Streams {
				if stream.Type == "Audio" {
					return stream.Index != audio.Index
				}
			}
		}
	}
	// Unknown metadata never satisfies a required condition.
	return nil
}
