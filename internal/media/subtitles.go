package media

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"unicode/utf8"
)

const (
	maxSidecarSubtitles = 32
	maxSubtitleBytes    = 10 << 20
)

// sidecarCodec returns the codec of a subtitle file Coach can deliver, or "".
func sidecarCodec(ext string) string {
	switch ext {
	case ".srt":
		return "srt"
	case ".vtt":
		return "webvtt"
	}
	return ""
}

// attachSidecars adds subtitle files to the video they sit next to, as Emby
// names them: "Movie.srt", "Movie.en.srt", "Movie.en.forced.srt" belong to
// "Movie.mp4". A file whose name extends two videos' names goes to the longer
// one. External streams are numbered after the embedded ones, in path order,
// so indexes stay stable across scans.
func attachSidecars(items []Item, subtitles []string) {
	slices.Sort(subtitles)
	for _, subtitle := range subtitles {
		dir, name := path.Dir(subtitle), path.Base(subtitle)
		name = strings.TrimSuffix(name, path.Ext(name))
		best, bestLen, rest := -1, 0, ""
		for i := range items {
			if path.Dir(items[i].Path) != dir {
				continue
			}
			base := path.Base(items[i].Path)
			base = strings.TrimSuffix(base, path.Ext(base))
			if (name == base || strings.HasPrefix(name, base+".")) && len(base) > bestLen {
				best, bestLen, rest = i, len(base), strings.TrimPrefix(strings.TrimPrefix(name, base), ".")
			}
		}
		if best < 0 || externalCount(items[best]) >= maxSidecarSubtitles {
			continue
		}
		items[best].Streams = append(items[best].Streams, sidecarStream(items[best], subtitle, rest))
	}
}

func externalCount(item Item) int {
	count := 0
	for _, s := range item.Streams {
		if s.Path != "" {
			count++
		}
	}
	return count
}

var languageTag = regexp.MustCompile(`^[a-z]{2,3}(-[a-z]{2})?$`)

func sidecarStream(item Item, subtitle, tags string) Stream {
	index := 0
	for _, s := range item.Streams {
		index = max(index, s.Index+1)
	}
	stream := Stream{Index: index, Type: "Subtitle", Codec: sidecarCodec(strings.ToLower(path.Ext(subtitle))), Path: subtitle}
	for _, tag := range strings.Split(strings.ToLower(tags), ".") {
		switch {
		case tag == "forced":
			stream.IsForced = true
		case tag == "default":
			stream.IsDefault = true
		case tag == "sdh" || tag == "hi" || tag == "cc":
			stream.IsHearingImpaired = true
		case stream.Language == "" && languageTag.MatchString(tag):
			stream.Language = tag
		}
	}
	return stream
}

// OpenSubtitle returns an external subtitle of item as WebVTT.
func (c *Catalog) OpenSubtitle(item Item, index int) ([]byte, error) {
	if c == nil || c.root == nil {
		return nil, errors.New("catalogue has no open root")
	}
	i := slices.IndexFunc(item.Streams, func(s Stream) bool { return s.Index == index && s.Path != "" })
	if i < 0 {
		return nil, os.ErrNotExist
	}
	stream := item.Streams[i]
	file, err := c.root.OpenFile(filepath.FromSlash(stream.Path), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("subtitle file is unavailable")
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("subtitle file is unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxSubtitleBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxSubtitleBytes {
		return nil, errors.New("subtitle file is too large")
	}
	return toWebVTT(data, stream.Codec)
}

// srtTiming matches an SRT timestamp; WebVTT needs a dot before the
// milliseconds and at least two hour digits.
var srtTiming = regexp.MustCompile(`\b(\d{1,2}):(\d{2}):(\d{2}),(\d{3})\b`)

// toWebVTT converts SRT to WebVTT and passes WebVTT through. Only UTF-8 (with
// or without a byte order mark) is accepted: guessing a legacy code page would
// show the wrong letters rather than fail visibly.
func toWebVTT(data []byte, codec string) ([]byte, error) {
	data = bytes.TrimPrefix(data, []byte("\ufeff"))
	if !utf8.Valid(data) {
		return nil, errors.New("subtitle file is not UTF-8")
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if codec == "webvtt" {
		if !strings.HasPrefix(text, "WEBVTT") {
			return nil, errors.New("subtitle file is not WebVTT")
		}
		return []byte(text), nil
	}
	var out strings.Builder
	out.WriteString("WEBVTT\n\n")
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "-->") {
			line = srtTiming.ReplaceAllStringFunc(line, func(t string) string {
				m := srtTiming.FindStringSubmatch(t)
				hours := m[1]
				if len(hours) == 1 {
					hours = "0" + hours
				}
				return hours + ":" + m[2] + ":" + m[3] + "." + m[4]
			})
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return []byte(out.String()), nil
}
