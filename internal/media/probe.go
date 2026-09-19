package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const maxProbeOutput = 1 << 20

type probeOutput struct {
	buffer bytes.Buffer
	cancel context.CancelFunc
}

func (b *probeOutput) Write(p []byte) (int, error) {
	if b.buffer.Len()+len(p) > maxProbeOutput {
		b.cancel()
		return 0, errors.New("probe output limit exceeded")
	}
	return b.buffer.Write(p)
}

func runProbe(ctx context.Context, binary string, file *os.File, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	// Do not inherit FFREPORT, credentials, or other application environment variables.
	cmd.Env = []string{"LC_ALL=C"}
	if file != nil {
		cmd.ExtraFiles = []*os.File{file}
	}
	output := &probeOutput{cancel: cancel}
	cmd.Stdout = output
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		return nil, errors.New("ffprobe failed or exceeded its limits")
	}
	return output.buffer.Bytes(), nil
}

func newProbe(ctx context.Context) (func(context.Context, *os.File) (Item, error), error) {
	binary, err := exec.LookPath("ffprobe")
	if err != nil {
		return nil, errors.New("-media-dir requires ffprobe on PATH")
	}
	protocols, err := runProbe(ctx, binary, nil, "-v", "error", "-protocols")
	if err != nil || !strings.Contains(string(protocols), "\n  fd\n") {
		return nil, errors.New("ffprobe must support the fd input protocol")
	}
	return func(ctx context.Context, file *os.File) (Item, error) {
		data, err := runProbe(ctx, binary, file, "-v", "error", "-max_alloc", "67108864",
			"-protocol_whitelist", "fd", "-threads", "1", "-probesize", "5000000", "-analyzeduration", "5000000",
			"-max_streams", "64", "-fd", "3", "-i", "fd:", "-of", "json",
			// Clients decide direct play from codec profile, level and pixel
			// format, so those are probed alongside the basic descriptors.
			"-show_entries", "format=duration:stream=index,codec_type,codec_name,codec_tag_string,profile,level,width,height,channels,channel_layout,sample_rate,bit_rate,pix_fmt,bits_per_raw_sample,refs,avg_frame_rate,r_frame_rate,time_base,field_order:stream_tags=language,title:stream_disposition=default,forced,attached_pic,hearing_impaired")
		if err != nil {
			return Item{}, err
		}
		return parseProbe(data)
	}, nil
}

// profileName normalises ffprobe's profile field, which is a string for most
// codecs but a numeric identifier for a few.
func profileName(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case float64:
		return strconv.FormatInt(int64(v), 10)
	}
	return ""
}

// frameRate converts ffprobe's "num/den" rational to frames per second.
func frameRate(value string) float64 {
	numerator, denominator, ok := strings.Cut(value, "/")
	if !ok {
		return 0
	}
	n, err := strconv.ParseFloat(numerator, 64)
	d, err2 := strconv.ParseFloat(denominator, 64)
	if err != nil || err2 != nil || d == 0 {
		return 0
	}
	return n / d
}

func parseProbe(data []byte) (Item, error) {
	var result struct {
		Format  struct{ Duration string }
		Streams []struct {
			Index         int
			CodecType     string  `json:"codec_type"`
			CodecName     string  `json:"codec_name"`
			CodecTag      string  `json:"codec_tag_string"`
			Profile       any     `json:"profile"`
			Level         float64 `json:"level"`
			Width         int
			Height        int
			Channels      int
			ChannelLayout string `json:"channel_layout"`
			SampleRate    string `json:"sample_rate"`
			BitRate       string `json:"bit_rate"`
			PixelFormat   string `json:"pix_fmt"`
			BitDepth      string `json:"bits_per_raw_sample"`
			RefFrames     int    `json:"refs"`
			AvgFrameRate  string `json:"avg_frame_rate"`
			RFrameRate    string `json:"r_frame_rate"`
			TimeBase      string `json:"time_base"`
			FieldOrder    string `json:"field_order"`
			Tags          struct {
				Language string
				Title    string
			}
			Disposition struct {
				Default         int
				Forced          int
				AttachedPic     int `json:"attached_pic"`
				HearingImpaired int `json:"hearing_impaired"`
			}
		}
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return Item{}, errors.New("invalid ffprobe output")
	}
	item := Item{Streams: []Stream{}}
	if seconds, err := strconv.ParseFloat(result.Format.Duration, 64); err == nil && seconds > 0 && seconds < float64(math.MaxInt64)/10000000 {
		item.RunTimeTicks = int64(seconds * 10000000)
	}
	video := false
	for _, raw := range result.Streams {
		stream := Stream{Index: raw.Index, Codec: raw.CodecName, Language: raw.Tags.Language,
			Title: raw.Tags.Title, CodecTag: raw.CodecTag, TimeBase: raw.TimeBase,
			Profile: profileName(raw.Profile), Level: raw.Level,
			IsDefault: raw.Disposition.Default != 0, IsForced: raw.Disposition.Forced != 0,
			IsHearingImpaired: raw.Disposition.HearingImpaired != 0,
			IsInterlaced:      raw.FieldOrder != "" && raw.FieldOrder != "progressive"}
		switch raw.CodecType {
		case "video":
			if raw.Disposition.AttachedPic != 0 {
				continue
			}
			video = true
			stream.Type, stream.Width, stream.Height = "Video", raw.Width, raw.Height
			stream.PixelFormat, stream.RefFrames = raw.PixelFormat, raw.RefFrames
			stream.BitDepth, _ = strconv.Atoi(raw.BitDepth)
			stream.AverageFrameRate = frameRate(raw.AvgFrameRate)
			stream.RealFrameRate = frameRate(raw.RFrameRate)
		case "audio":
			stream.Type, stream.Channels = "Audio", raw.Channels
			stream.ChannelLayout = raw.ChannelLayout
			stream.SampleRate, _ = strconv.Atoi(raw.SampleRate)
		case "subtitle":
			stream.Type = "Subtitle"
		default:
			continue
		}
		stream.BitRate, _ = strconv.ParseInt(raw.BitRate, 10, 64)
		item.Streams = append(item.Streams, stream)
	}
	if !video {
		return Item{}, errors.New("no video stream")
	}
	return item, nil
}
