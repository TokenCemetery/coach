package media

import (
	"bytes"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestPrimaryImagesBoundedAndConfined(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	catalog := &Catalog{root: root}
	defer catalog.Close()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 3))); err != nil {
		t.Fatal(err)
	}
	poster := filepath.Join(dir, "Movie-poster.png")
	if err := os.WriteFile(poster, encoded.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	item := Item{ID: "movie", Path: "Movie.mp4"}
	file, info, err := catalog.OpenImage(item)
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(file)
	file.Close()
	if err != nil || !bytes.Equal(content, encoded.Bytes()) || info.Width != 2 || info.Height != 3 || info.MIME != "image/png" {
		t.Fatal("incorrect image")
	}
	changed := time.Now().Add(time.Second)
	if err := os.Chtimes(poster, changed, changed); err != nil {
		t.Fatal(err)
	}
	updated, ok := catalog.PrimaryImage(item)
	if !ok || updated.Revision == info.Revision {
		t.Fatal("stale image revision")
	}
	if err := os.Remove(poster); err != nil {
		t.Fatal(err)
	}
	if _, found := catalog.PrimaryImage(item); found {
		t.Fatal("removed image advertised")
	}
	outside := filepath.Join(t.TempDir(), "private.png")
	if err := os.WriteFile(outside, encoded.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, poster); err != nil {
		t.Fatal(err)
	}
	if _, found := catalog.PrimaryImage(item); found {
		t.Fatal("symlink escaped root")
	}
	if err := os.Remove(poster); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(poster, 0600); err != nil {
		t.Fatal(err)
	}
	if _, found := catalog.PrimaryImage(item); found {
		t.Fatal("FIFO accepted")
	}
	if err := os.Remove(poster); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(poster)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxImageBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, found := catalog.PrimaryImage(item); found {
		t.Fatal("oversized image accepted")
	}
	if err := os.WriteFile(poster, []byte("<svg/>"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, found := catalog.PrimaryImage(item); found {
		t.Fatal("invalid image accepted")
	}
	// Directory sidecars work for virtual series and season nodes too.
	if err := os.Mkdir(filepath.Join(dir, "Show"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Show", "poster.png"), encoded.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if _, found := catalog.PrimaryImage(Item{Kind: "Series", Path: "Show"}); !found {
		t.Fatal("series poster missing")
	}
}
