package server

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TokenCemetery/coach/internal/media"
)

func streamFixture(t *testing.T, content []byte) (string, http.Handler) {
	t.Helper()
	name := filepath.Join(t.TempDir(), "movie.mp4")
	if err := os.WriteFile(name, content, 0600); err != nil {
		t.Fatal(err)
	}
	return name, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		file, err := os.Open(name)
		if err != nil {
			fail(w, 404, "MediaUnavailable")
			return
		}
		defer file.Close()
		serveMediaFile(w, r, file, media.Item{ID: "movie", Container: "mp4"})
	})
}

func TestStreamRangesAndValidators(t *testing.T) {
	name, h := streamFixture(t, []byte("0123456789"))
	for _, c := range []struct {
		method, byteRange, body, contentRange string
		status                                int
	}{
		{"GET", "", "0123456789", "", 200},
		{"HEAD", "", "", "", 200},
		{"GET", "bytes=0-2", "012", "bytes 0-2/10", 206},
		{"GET", "bytes=4-", "456789", "bytes 4-9/10", 206},
		{"GET", "bytes=-2", "89", "bytes 8-9/10", 206},
		{"GET", "bytes=10-", "invalid range: failed to overlap\n", "bytes */10", 416},
	} {
		r := httptest.NewRequest(c.method, "/", nil)
		r.Header.Set("Range", c.byteRange)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.status || w.Body.String() != c.body || w.Header().Get("Content-Range") != c.contentRange {
			t.Fatalf("%s %s: status=%d body=%q range=%q", c.method, c.byteRange, w.Code, w.Body.String(), w.Header().Get("Content-Range"))
		}
	}
	first := request(h, "GET", "/", "", "", "")
	etag := first.Header().Get("ETag")
	if !strings.HasPrefix(etag, `W/"`) || first.Header().Get("Content-Type") != "video/mp4" {
		t.Fatal("missing weak validator or video MIME")
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("If-None-Match", etag)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	expectStatus(t, w, 304)

	// The ID and path stay the same after replacement; validators must not.
	if err := os.WriteFile(name, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	modified := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(name, modified, modified); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	expectStatus(t, w, 200)
	if w.Body.String() != "replacement" || w.Header().Get("ETag") == etag || w.Header().Get("Last-Modified") != modified.UTC().Format(http.TimeFormat) {
		t.Fatal("response used stale file metadata")
	}
	r.Header.Del("If-None-Match")
	r.Header.Set("Range", "bytes=0-2")
	for _, validator := range []string{etag, w.Header().Get("ETag")} {
		r.Header.Set("If-Range", validator)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, r)
		expectStatus(t, w, 200)
		if w.Body.String() != "replacement" {
			t.Fatal("weak If-Range must return the full representation")
		}
	}
}

type delayedStreamWriter struct{ http.ResponseWriter }

func (w delayedStreamWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w delayedStreamWriter) Write(p []byte) (int, error) {
	time.Sleep(20 * time.Millisecond)
	return w.ResponseWriter.Write(p)
}

func TestStreamOutlivesServerWriteTimeout(t *testing.T) {
	content := bytes.Repeat([]byte("video"), 100000)
	_, h := streamFixture(t, content)
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(delayedStreamWriter{w}, r)
	}))
	s.Config.WriteTimeout = 100 * time.Millisecond
	s.Start()
	defer s.Close()
	client := s.Client()
	client.Timeout = 5 * time.Second
	started := time.Now()
	response, err := client.Get(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	got, err := io.ReadAll(response.Body)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("stream was truncated: bytes=%d error=%v", len(got), err)
	}
	if time.Since(started) <= s.Config.WriteTimeout {
		t.Fatal("test did not exceed the server write timeout")
	}
}

type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
	err       error
}

func (w *deadlineRecorder) SetWriteDeadline(deadline time.Time) error {
	w.deadlines = append(w.deadlines, deadline)
	return w.err
}

func TestStreamWriteDeadlineRenewalAndErrors(t *testing.T) {
	w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	stream := &streamWriter{ResponseWriter: w, controller: http.NewResponseController(w)}
	for range 2 {
		before := time.Now()
		if _, err := stream.Write([]byte("chunk")); err != nil {
			t.Fatal(err)
		}
		deadline := w.deadlines[len(w.deadlines)-1]
		if deadline.Before(before.Add(streamWriteTimeout)) || deadline.After(time.Now().Add(streamWriteTimeout)) {
			t.Fatal("write deadline does not bound the next write")
		}
	}
	if len(w.deadlines) != 2 || !w.deadlines[1].After(w.deadlines[0]) {
		t.Fatal("deadline was not renewed")
	}
	w.err = os.ErrDeadlineExceeded
	if n, err := stream.Write([]byte("blocked")); n != 0 || !errors.Is(err, os.ErrDeadlineExceeded) || w.Body.String() != "chunkchunk" {
		t.Fatal("deadline failure did not stop the write")
	}
}

type blockedStreamWriter struct {
	*httptest.ResponseRecorder
	conn net.Conn
}

func (w blockedStreamWriter) Write(p []byte) (int, error) { return w.conn.Write(p) }
func (w blockedStreamWriter) SetWriteDeadline(deadline time.Time) error {
	// Scale the production budget down without waiting 30 seconds in tests.
	return w.conn.SetWriteDeadline(time.Now().Add(time.Until(deadline) / 300))
}

func TestStreamBlockedWriteTimesOut(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	w := blockedStreamWriter{httptest.NewRecorder(), server}
	stream := &streamWriter{ResponseWriter: w, controller: http.NewResponseController(w)}
	done := make(chan error, 1)
	go func() {
		_, err := stream.Write([]byte("client never reads"))
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("blocked write: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked write was not bounded")
	}
}
