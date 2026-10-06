package media

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestScanReadOnlyStableAndIsolated(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Movie.MP4", "nested/Movie.mp4", "broken.mkv", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "outside")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "Movie.MP4"), filepath.Join(dir, "link.mp4")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe.mp4"), 0600); err != nil {
		t.Fatal(err)
	}
	probe := func(ctx context.Context, f *os.File) (Item, error) {
		if strings.HasSuffix(f.Name(), "broken.mkv") {
			return Item{}, errors.New("bad media")
		}
		return Item{RunTimeTicks: 10000000, Streams: []Stream{{Type: "Video", Codec: "h264"}}}, nil
	}
	first, err := scan(context.Background(), dir, probe)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 2 || first.Skipped != 1 {
		t.Fatalf("movies=%d skipped=%d", len(first.Items), first.Skipped)
	}
	defer func() { _ = first.Close() }()
	second, err := scan(context.Background(), dir, probe)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	// The open scan root differs between runs by construction; the catalogue
	// content it produces must not.
	if first.ID != second.ID || first.Skipped != second.Skipped || !reflect.DeepEqual(first.Items, second.Items) {
		t.Fatal("scan is not deterministic")
	}
	if first.Items[0].ID == first.Items[1].ID {
		t.Fatal("relative directory must distinguish IDs")
	}
	for _, item := range first.Items {
		if item.Name != "Movie" || item.Container != "mp4" || item.Size != 7 {
			t.Fatal("incorrect file metadata")
		}
	}
	content, err := os.ReadFile(filepath.Join(dir, "Movie.MP4"))
	if err != nil || string(content) != "fixture" {
		t.Fatal("source was modified")
	}
	if _, err := scan(context.Background(), filepath.Join(dir, "missing"), probe); err == nil {
		t.Fatal("missing directory accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := scan(ctx, dir, probe); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestScanDepthLimit(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, strings.Repeat("a/", 67)), 0700); err != nil {
		t.Fatal(err)
	}
	_, err := scan(context.Background(), dir, func(context.Context, *os.File) (Item, error) { t.Fatal("unexpected probe"); return Item{}, nil })
	if err == nil {
		t.Fatal("depth limit not enforced")
	}
}

func TestRescan(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("Kept.mp4", "kept")
	write("Kept.srt", "1\n00:00:01,000 --> 00:00:02,000\nHi\n")
	write("Changed.mp4", "old")
	write("Removed.mp4", "removed")
	write("Show/Season 1/Show.S01E01.mp4", "episode")
	probed := map[string]int{}
	probe := func(ctx context.Context, f *os.File) (Item, error) {
		probed[filepath.Base(f.Name())]++
		return Item{RunTimeTicks: 10000000, Streams: []Stream{{Index: 0, Type: "Video", Codec: "h264"}}}, nil
	}
	first, err := scan(context.Background(), dir, probe)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	ids := map[string]Item{}
	for _, item := range first.Items {
		ids[item.Path] = item
		if !item.Added.Equal(item.Modified) {
			t.Fatalf("%s: added %v, want mtime %v", item.Path, item.Added, item.Modified)
		}
	}

	write("Changed.mp4", "changed content")
	write("New.mp4", "new")
	if err := os.Remove(filepath.Join(dir, "Removed.mp4")); err != nil {
		t.Fatal(err)
	}
	clear(probed)
	start := time.Now().UTC()
	second, err := first.Rescan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]int{"Changed.mp4": 1, "New.mp4": 1}; !reflect.DeepEqual(probed, want) {
		t.Fatalf("probed %v, want %v", probed, want)
	}
	paths := map[string]Item{}
	for _, item := range second.Items {
		paths[item.Path] = item
	}
	if len(paths) != 4 || paths["Removed.mp4"].ID != "" {
		t.Fatalf("items %v", paths)
	}
	for _, path := range []string{"Kept.mp4", "Changed.mp4", "Show/Season 1/Show.S01E01.mp4"} {
		if paths[path].ID != ids[path].ID || !paths[path].Added.Equal(ids[path].Added) {
			t.Fatalf("%s lost its ID or date added", path)
		}
	}
	if added := paths["New.mp4"].Added; added.Before(start) {
		t.Fatalf("new file added %v, before rescan %v", added, start)
	}
	// The reused item gets its sidecar once, not once per scan.
	if !reflect.DeepEqual(paths["Kept.mp4"].Streams, ids["Kept.mp4"].Streams) || len(paths["Kept.mp4"].Streams) != 2 {
		t.Fatalf("kept streams %v", paths["Kept.mp4"].Streams)
	}
	episode := paths["Show/Season 1/Show.S01E01.mp4"]
	if episode.Kind != "Episode" || len(second.Folders) != 2 || len(second.Episodes(episode.SeriesID)) != 1 {
		t.Fatalf("episode %v folders %v", episode, second.Folders)
	}
	if len(first.Items) != 4 || paths["Changed.mp4"].Size == ids["Changed.mp4"].Size {
		t.Fatal("rescan changed the previous catalog or missed the change")
	}

	// A walk error keeps the previous catalog.
	if err := os.Chmod(filepath.Join(dir, "Show"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Rescan(context.Background()); err == nil {
		t.Fatal("unreadable directory published")
	}
	if err := os.Chmod(filepath.Join(dir, "Show"), 0700); err != nil { //nolint:gosec // a directory needs its search bit
		t.Fatal(err)
	}
	// So does a volume that suddenly has no videos.
	for _, name := range []string{"Kept.mp4", "Changed.mp4", "New.mp4", "Show/Season 1/Show.S01E01.mp4"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := second.Rescan(context.Background()); err == nil {
		t.Fatal("empty volume published")
	}
	// The shared root still serves files after the failed rescans.
	write("Kept.mp4", "kept")
	if f, err := second.Open(Item{Path: "Kept.mp4"}); err != nil {
		t.Fatal(err)
	} else {
		_ = f.Close()
	}
}
