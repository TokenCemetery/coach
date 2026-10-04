package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

const subtitle = "1\n00:00:00,000 --> 00:00:02,000\nCoach e2e subtitle\n"

// generateMedia writes the synthetic library: a movie with two audio tracks,
// an embedded and an external subtitle and a poster; a series with a regular
// and a special season; and an audio track and a photo, which Coach does not
// catalog yet but the contract covers.
func generateMedia(root string) error {
	movieDir := filepath.Join(root, "Movies", "Sample Movie (2024)")
	showDir := filepath.Join(root, "Sample Show")
	dirs := []string{movieDir, filepath.Join(showDir, "Season 01"), filepath.Join(showDir, "Season 00"),
		filepath.Join(root, "Music", "Sample Artist", "Sample Album"), filepath.Join(root, "Photos")}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return err
		}
	}
	srt := filepath.Join(movieDir, "Sample Movie (2024).eng.srt")
	if err := os.WriteFile(srt, []byte(subtitle), 0o600); err != nil {
		return err
	}
	movie := filepath.Join(movieDir, "Sample Movie (2024).mp4")
	jobs := [][]string{
		{"-f", "lavfi", "-i", "testsrc=size=320x240:rate=25:duration=4",
			"-f", "lavfi", "-i", "sine=frequency=440:duration=4",
			"-f", "lavfi", "-i", "sine=frequency=880:duration=4",
			"-i", srt,
			"-map", "0:v", "-map", "1:a", "-map", "2:a", "-map", "3:s",
			"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-c:s", "mov_text",
			"-metadata:s:a:0", "language=eng", "-metadata:s:a:1", "language=rus", "-metadata:s:s:0", "language=eng",
			movie},
		poster(filepath.Join(movieDir, "Sample Movie (2024)-poster.jpg")),
		poster(filepath.Join(showDir, "poster.jpg")),
		episode(filepath.Join(showDir, "Season 01", "Sample Show.S01E01.mp4")),
		episode(filepath.Join(showDir, "Season 01", "Sample Show.S01E02.mp4")),
		episode(filepath.Join(showDir, "Season 00", "Sample Show.S00E01.mp4")),
		{"-f", "lavfi", "-i", "sine=frequency=330:duration=2", "-c:a", "aac",
			filepath.Join(root, "Music", "Sample Artist", "Sample Album", "01 - Sample Track.m4a")},
		poster(filepath.Join(root, "Photos", "Sample Photo.jpg")),
	}
	for _, args := range jobs {
		cmd := exec.CommandContext(context.Background(), "ffmpeg", append([]string{"-v", "error", "-nostdin", "-y"}, args...)...) //nolint:gosec // arguments are fixed lavfi sources and temp paths
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("ffmpeg %s: %w\n%s (FFmpeg with libx264 and FFprobe are required)", args[len(args)-1], err, out)
		}
	}
	return nil
}

func poster(path string) []string {
	return []string{"-f", "lavfi", "-i", "color=c=blue:s=48x72", "-frames:v", "1", path}
}

func episode(path string) []string {
	return []string{"-f", "lavfi", "-i", "testsrc=size=160x120:rate=25:duration=2",
		"-f", "lavfi", "-i", "anullsrc=r=48000:cl=stereo", "-t", "2",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", path}
}
