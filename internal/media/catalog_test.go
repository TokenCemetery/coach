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
	defer first.Close()
	second, err := scan(context.Background(), dir, probe)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
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
