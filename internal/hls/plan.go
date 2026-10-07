// Package hls remuxes a video into HLS with MPEG-TS segments: FFmpeg copies
// the video and copies or encodes the audio, and the playlist is cut on the
// source's keyframes so that it matches the segments FFmpeg writes.
package hls

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// SegmentSeconds is the minimum segment length; segments end on the first
// keyframe at least this far from their start.
const SegmentSeconds = 6

const (
	// seekStep spaces the seeks that list keyframes. A seek lands on the
	// keyframe at or before its target, so seeking once per segment would
	// skip keyframes and make segments longer than SegmentSeconds.
	seekStep      = SegmentSeconds / 3
	keyframeLimit = 5 * time.Minute
	maxKeyframes  = 1 << 20
)

// Boundaries returns segment start times, relative to the file's start time,
// for keyframes in ascending order. The first segment starts at the first
// keyframe.
func Boundaries(keyframes []float64) []float64 {
	if len(keyframes) == 0 {
		return nil
	}
	bounds := []float64{keyframes[0]}
	for _, k := range keyframes[1:] {
		if k-bounds[len(bounds)-1] >= SegmentSeconds {
			bounds = append(bounds, k)
		}
	}
	return bounds
}

// Playlist renders the VOD media playlist for bounds; duration is the length
// of the video in seconds.
func Playlist(bounds []float64, duration float64, segmentURL func(n int) string) string {
	durations := make([]float64, len(bounds))
	target := 0.0
	for n, start := range bounds {
		end := duration
		if n+1 < len(bounds) {
			end = bounds[n+1]
		}
		durations[n] = max(end-start, 0.001)
		target = max(target, durations[n])
	}
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:0\n", int(target+0.999))
	for n, d := range durations {
		fmt.Fprintf(&b, "#EXTINF:%.6f,\n%s\n", d, segmentURL(n))
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}

// keyframes lists keyframe times of one video stream, relative to the
// file's start time, and returns that start time. It seeks every
// seekStep seconds and keeps the keyframe each seek lands on, so it reads a
// few packets per seek instead of the whole file. Any subset of the keyframes gives a
// plan FFmpeg follows exactly. Containers whose seeks do not land on
// keyframes (MPEG-TS) fall back to reading every packet. The result is
// cached by the Manager.
func keyframes(ctx context.Context, ffprobe string, file *os.File, stream int) ([]float64, float64, error) {
	ctx, cancel := context.WithTimeout(ctx, keyframeLimit)
	defer cancel()
	start, duration := 0.0, 0.0
	err := probe(ctx, ffprobe, file, []string{"-show_entries", "format=start_time,duration"}, func(fields []string) error {
		if len(fields) >= 3 && fields[0] == "format" {
			start, _ = strconv.ParseFloat(fields[1], 64)
			duration, _ = strconv.ParseFloat(fields[2], 64)
		}
		return nil
	})
	if err != nil {
		return nil, 0, errors.New("cannot list keyframes")
	}
	selected := []string{"-select_streams", strconv.Itoa(stream), "-show_entries", "packet=pts_time,flags"}
	var times []float64
	if duration > 0 && duration/seekStep < maxKeyframes {
		intervals := make([]string, 0, int(duration/seekStep)+1)
		for t := 0.0; t < duration; t += seekStep {
			intervals = append(intervals, strconv.FormatFloat(start+t, 'f', 3, 64)+"%+#1")
		}
		seeked := true
		err = probe(ctx, ffprobe, file, append(selected, "-read_intervals", strings.Join(intervals, ",")), func(fields []string) error {
			t, key, ok := packet(fields)
			if ok && !key {
				seeked = false
			}
			if ok && key {
				times = append(times, t)
			}
			return nil
		})
		if err != nil {
			return nil, 0, errors.New("cannot list keyframes")
		}
		if !seeked {
			times = nil
		}
	}
	if len(times) == 0 {
		err = probe(ctx, ffprobe, file, selected, func(fields []string) error {
			if t, key, ok := packet(fields); ok && key {
				if len(times) >= maxKeyframes {
					return errors.New("too many keyframes")
				}
				times = append(times, t)
			}
			return nil
		})
		if err != nil || len(times) == 0 {
			return nil, 0, errors.New("cannot list keyframes")
		}
	}
	// Seeks list keyframes in time order and packets come in decode order;
	// a stray one out of order or repeated would break the segment plan.
	result := times[:0]
	for _, t := range times {
		if t-start >= 0 && (len(result) == 0 || t-start > result[len(result)-1]) {
			result = append(result, t-start)
		}
	}
	return result, start, nil
}

// packet parses a CSV packet line with pts_time and flags.
func packet(fields []string) (t float64, key, ok bool) {
	if len(fields) < 3 || fields[0] != "packet" {
		return 0, false, false
	}
	t, err := strconv.ParseFloat(fields[1], 64)
	if err != nil {
		return 0, false, false // N/A: the packet has no timestamp
	}
	return t, strings.HasPrefix(fields[2], "K"), true
}

// probe runs FFprobe on file from its start with CSV output and passes each
// line's fields to line; an error from line stops FFprobe.
func probe(ctx context.Context, ffprobe string, file *os.File, args []string, line func(fields []string) error) error {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	args = append([]string{"-v", "error", "-protocol_whitelist", "fd", "-fd", "3"}, args...)
	cmd := exec.CommandContext(ctx, ffprobe, append(args, "-of", "csv", "fd:")...) //nolint:gosec // binary is ffprobe from PATH; arguments are numbers and fixed options
	cmd.Env = []string{"LC_ALL=C"}
	cmd.ExtraFiles = []*os.File{file}
	cmd.WaitDelay = time.Second
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	var lineErr error
	lines := bufio.NewScanner(out)
	for lineErr == nil && lines.Scan() {
		lineErr = line(strings.Split(lines.Text(), ","))
	}
	if lineErr == nil {
		lineErr = lines.Err()
	}
	if lineErr != nil {
		cancel()
	}
	_, _ = io.Copy(io.Discard, out)
	if err := cmd.Wait(); err != nil {
		return err
	}
	return lineErr
}
