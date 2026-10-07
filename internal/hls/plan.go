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

// keyframes lists the keyframe times of one video stream, relative to the
// file's start time, and returns that start time. FFprobe reads every
// packet, so this reads the whole file; the result is cached by the Manager.
func keyframes(ctx context.Context, ffprobe string, file *os.File, stream int) ([]float64, float64, error) {
	ctx, cancel := context.WithTimeout(ctx, keyframeLimit)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffprobe, "-v", "error", //nolint:gosec // binary is ffprobe from PATH; the only variable argument is an integer
		"-protocol_whitelist", "fd", "-fd", "3", "-select_streams", strconv.Itoa(stream),
		"-show_entries", "packet=pts_time,flags:format=start_time", "-of", "csv", "fd:")
	cmd.Env = []string{"LC_ALL=C"}
	cmd.ExtraFiles = []*os.File{file}
	cmd.WaitDelay = time.Second
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, 0, err
	}
	if err := cmd.Start(); err != nil {
		return nil, 0, err
	}
	var times []float64
	start, parseErr := 0.0, error(nil)
	lines := bufio.NewScanner(out)
	for lines.Scan() {
		fields := strings.Split(lines.Text(), ",")
		switch {
		case len(fields) >= 3 && fields[0] == "packet" && strings.HasPrefix(fields[2], "K"):
			t, err := strconv.ParseFloat(fields[1], 64)
			if err != nil {
				continue // N/A: the packet has no timestamp
			}
			if len(times) >= maxKeyframes {
				parseErr = errors.New("too many keyframes")
				cancel()
			}
			times = append(times, t)
		case len(fields) >= 2 && fields[0] == "format":
			start, _ = strconv.ParseFloat(fields[1], 64)
		}
	}
	if lines.Err() != nil {
		parseErr = lines.Err()
		cancel()
	}
	_, _ = io.Copy(io.Discard, out)
	if err := cmd.Wait(); err != nil || parseErr != nil || len(times) == 0 {
		return nil, 0, errors.New("cannot list keyframes")
	}
	// Packets are in decode order; keyframe times are ascending in practice,
	// but a stray one out of order would break the segment plan.
	result := times[:0]
	for _, t := range times {
		if t-start >= 0 && (len(result) == 0 || t-start > result[len(result)-1]) {
			result = append(result, t-start)
		}
	}
	return result, start, nil
}
