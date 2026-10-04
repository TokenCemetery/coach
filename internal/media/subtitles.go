package media

import (
	"bytes"
	"context"
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
// one; videos that share that name ("Movie.mp4", "Movie.mkv") all get it. External streams are numbered after the embedded ones, in path order,
// so indexes stay stable across scans.
func attachSidecars(items []Item, subtitles []string) {
	slices.Sort(subtitles)
	for _, subtitle := range subtitles {
		dir, name := path.Dir(subtitle), path.Base(subtitle)
		name = strings.TrimSuffix(name, path.Ext(name))
		// Every video with the longest matching name gets the file, so
		// "Movie.mp4" and "Movie.mkv" both get "Movie.en.srt". Case is
		// ignored, as media folders often mix it.
		matches, bestLen := []int{}, 0
		for i := range items {
			if path.Dir(items[i].Path) != dir {
				continue
			}
			base := path.Base(items[i].Path)
			base = strings.TrimSuffix(base, path.Ext(base))
			if len(name) < len(base) || !strings.EqualFold(name[:len(base)], base) || (len(name) > len(base) && name[len(base)] != '.') {
				continue
			}
			if len(base) > bestLen {
				matches, bestLen = matches[:0], len(base)
			}
			if len(base) == bestLen {
				matches = append(matches, i)
			}
		}
		rest := strings.TrimPrefix(name[min(bestLen, len(name)):], ".")
		for _, i := range matches {
			if externalCount(items[i]) < maxSidecarSubtitles {
				items[i].Streams = append(items[i].Streams, sidecarStream(items[i], subtitle, rest))
			}
		}
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

// SubtitleDeliverable reports whether OpenSubtitle can serve the stream:
// a sidecar file, or an embedded text track when FFmpeg is available.
func (c *Catalog) SubtitleDeliverable(item Item, index int) bool {
	i := slices.IndexFunc(item.Streams, func(s Stream) bool { return s.Index == index && s.Type == "Subtitle" })
	if i < 0 {
		return false
	}
	return item.Streams[i].Path != "" || (c != nil && c.extractor != nil && textSubtitleCodec(item.Streams[i].Codec))
}

// textSubtitleCodec lists embedded codecs FFmpeg converts to WebVTT. Bitmap
// subtitles (PGS, DVD, DVB) would need burning into the video.
func textSubtitleCodec(codec string) bool {
	switch strings.ToLower(codec) {
	case "subrip", "srt", "mov_text", "webvtt", "ass", "ssa", "text":
		return true
	}
	return false
}

// OpenSubtitle returns a subtitle stream of item as WebVTT.
func (c *Catalog) OpenSubtitle(ctx context.Context, item Item, index int) ([]byte, error) {
	if c == nil || c.root == nil || !c.SubtitleDeliverable(item, index) {
		return nil, os.ErrNotExist
	}
	stream := item.Streams[slices.IndexFunc(item.Streams, func(s Stream) bool { return s.Index == index && s.Type == "Subtitle" })]
	if stream.Path == "" {
		file, err := c.Open(item)
		if err != nil {
			return nil, err
		}
		defer func() { _ = file.Close() }()
		return c.extractor.extract(ctx, file, index)
	}
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

var srtTiming = regexp.MustCompile(`\b(\d{1,2}):(\d{2}):(\d{2}),(\d{3})\b`)

// toWebVTT converts SRT to WebVTT and passes WebVTT through. Only UTF-8 (with
// or without a byte order mark) is accepted: guessing a legacy code page would
// show the wrong letters rather than fail visibly.
func toWebVTT(data []byte, codec string) ([]byte, error) {
	data = bytes.TrimPrefix(data, []byte("\ufeff"))
	if !utf8.Valid(data) {
		return nil, errors.New("subtitle file is not UTF-8")
	}
	// Old SRT files may end lines with a bare CR.
	text := strings.ReplaceAll(strings.ReplaceAll(string(data), "\r\n", "\n"), "\r", "\n")
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
