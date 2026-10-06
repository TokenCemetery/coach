package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/TokenCemetery/coach/internal/media"
)

func TestRescanLoop(t *testing.T) {
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " required")
		}
	}
	dir := t.TempDir()
	movie := filepath.Join(dir, "First.mp4")
	build := exec.CommandContext(context.Background(), "ffmpeg", "-v", "error", "-nostdin", "-f", "lavfi", "-i", "color=c=black:s=64x64:d=1", "-c:v", "libx264", "-pix_fmt", "yuv420p", movie) //nolint:gosec // fixed arguments and temp paths
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("cannot build fixture: %v %s", err, out)
	}
	catalog, err := media.Scan(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = catalog.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hangup := make(chan os.Signal, 1)
	published := make(chan *media.Catalog, 1)
	done := make(chan struct{})
	go func() {
		rescanLoop(ctx, catalog, hangup, 0, func(c *media.Catalog) { published <- c })
		close(done)
	}()

	data, err := os.ReadFile(movie)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Second.mp4"), data, 0600); err != nil { //nolint:gosec // test temp directory
		t.Fatal(err)
	}
	hangup <- syscall.SIGHUP
	select {
	case next := <-published:
		if len(next.Items) != 2 {
			t.Fatalf("published %d videos, want 2", len(next.Items))
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no catalog published after SIGHUP")
	}

	// An emptied directory is not published; the loop keeps running.
	for _, name := range []string{"First.mp4", "Second.mp4"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	hangup <- syscall.SIGHUP
	select {
	case next := <-published:
		t.Fatalf("published %d videos from an empty directory", len(next.Items))
	case <-time.After(time.Second):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("loop did not stop")
	}
}
