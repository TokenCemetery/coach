package media

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestScanAttachesSidecarSubtitles(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"Movie.mp4":              "video",
		"Movie.Part2.mp4":        "video",
		"Movie.srt":              "1\r\n00:00:01,000 --> 00:00:02,500\r\nПривет\r\n",
		"Movie.en.forced.srt":    "1\n00:00:01,000 --> 00:00:02,000\nHi\n",
		"Movie.Part2.ru.SDH.srt": "1\n00:00:01,000 --> 00:00:02,000\nЧасть 2\n",
		"Movie.vtt":              "WEBVTT\n\n00:01.000 --> 00:02.000\nVTT\n",
		"Orphan.srt":             "1\n00:00:01,000 --> 00:00:02,000\nnobody\n",
		"nested/Movie.srt":       "1\n00:00:01,000 --> 00:00:02,000\nother folder\n",
		"Movie.cp1251.srt":       "1\n00:00:01,000 --> 00:00:02,000\n\xcf\xf0\xe8\xe2\xe5\xf2\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	probe := func(context.Context, *os.File) (Item, error) {
		return Item{RunTimeTicks: 10000000, Streams: []Stream{{Index: 0, Type: "Video", Codec: "h264"}, {Index: 1, Type: "Audio", Codec: "aac"}}}, nil
	}
	catalog, err := scan(context.Background(), dir, probe)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = catalog.Close() }()
	byPath := map[string]Item{}
	for _, item := range catalog.Items {
		byPath[item.Path] = item
	}
	describe := func(item Item) string {
		parts := []string{}
		for _, s := range item.Streams[2:] {
			flags := ""
			if s.IsForced {
				flags += "+forced"
			}
			if s.IsHearingImpaired {
				flags += "+sdh"
			}
			parts = append(parts, strings.Join([]string{strconv.Itoa(s.Index), s.Path, s.Codec, s.Language + flags}, "|"))
		}
		return strings.Join(parts, ";")
	}
	// Sorted by path; Movie.Part2.* belongs to the longer name, not to Movie.
	want := "2|Movie.cp1251.srt|srt|;3|Movie.en.forced.srt|srt|en+forced;4|Movie.srt|srt|;5|Movie.vtt|webvtt|"
	if got := describe(byPath["Movie.mp4"]); got != want {
		t.Fatalf("Movie streams:\n got %s\nwant %s", got, want)
	}
	if got := describe(byPath["Movie.Part2.mp4"]); got != "2|Movie.Part2.ru.SDH.srt|srt|ru+sdh" {
		t.Fatalf("Part2 streams: %s", got)
	}
	movie := byPath["Movie.mp4"]
	for index, want := range map[int]string{
		4: "WEBVTT\n\n1\n00:00:01.000 --> 00:00:02.500\nПривет\n",
		5: "WEBVTT\n\n00:01.000 --> 00:02.000\nVTT\n",
	} {
		got, err := catalog.OpenSubtitle(context.Background(), movie, index)
		if err != nil || !strings.HasPrefix(string(got), want) {
			t.Fatalf("subtitle %d: %q, %v", index, got, err)
		}
	}
	for _, index := range []int{0, 2, 9} {
		if _, err := catalog.OpenSubtitle(context.Background(), movie, index); err == nil {
			t.Fatalf("subtitle %d: want an error (embedded, not UTF-8 or missing)", index)
		}
	}
}

func TestSRTTimestampsGetTwoHourDigits(t *testing.T) {
	got, err := toWebVTT([]byte("1\n1:02:03,004 --> 12:00:00,000\ntext 1:02:03,004\n"), "srt")
	if err != nil {
		t.Fatal(err)
	}
	if want := "WEBVTT\n\n1\n01:02:03.004 --> 12:00:00.000\ntext 1:02:03,004\n\n"; string(got) != want {
		t.Fatalf("got %q", got)
	}
}

func TestEmbeddedSubtitleExtraction(t *testing.T) {
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " required")
		}
	}
	dir := t.TempDir()
	srt := filepath.Join(t.TempDir(), "in.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:00,000 --> 00:00:01,000\nEmbedded cue\n"), 0600); err != nil {
		t.Fatal(err)
	}
	build := exec.CommandContext(context.Background(), "ffmpeg", "-v", "error", "-nostdin", "-f", "lavfi", "-i", "color=c=black:s=64x64:d=2", "-i", srt, //nolint:gosec // fixed arguments and temp paths
		"-map", "0:v", "-map", "1:s", "-c:v", "libx264", "-c:s", "srt", filepath.Join(dir, "Movie.mkv"))
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("cannot build fixture: %v %s", err, out)
	}
	catalog, err := Scan(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = catalog.Close() }()
	movie := catalog.Items[0]
	if !catalog.SubtitleDeliverable(movie, 1) || catalog.SubtitleDeliverable(movie, 0) {
		t.Fatalf("deliverable streams wrong: %+v", movie.Streams)
	}
	got, err := catalog.OpenSubtitle(context.Background(), movie, 1)
	if err != nil || !strings.HasPrefix(string(got), "WEBVTT") || !strings.Contains(string(got), "Embedded cue") {
		t.Fatalf("extracted: %q, %v", got, err)
	}
	// With every slot taken, a request waits and gives up with its context.
	for range maxSubtitleJobs {
		catalog.extractor.slots <- struct{}{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := catalog.OpenSubtitle(ctx, movie, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued request: %v, want deadline exceeded", err)
	}
	for range maxSubtitleJobs {
		<-catalog.extractor.slots
	}
	// A crashing or failing FFmpeg is an error, never an empty success.
	if _, err := catalog.extractor.extract(context.Background(), mustOpen(t, srt), 5); err == nil {
		t.Fatal("missing stream extracted without error")
	}
}

func mustOpen(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}
