package server

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/TokenCemetery/coach/internal/media"
)

func TestSignedImageHTTP(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe required to open media root")
	}
	dir := t.TempDir()
	catalog, err := media.Scan(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	catalog.Items = []media.Item{{ID: "movie", Path: "Movie.mp4"}, {ID: "other", Path: "Other.mp4"}}
	var content bytes.Buffer
	if err := png.Encode(&content, image.NewRGBA(image.Rect(0, 0, 2, 3))); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(dir, "Movie-poster.png")
	if err := os.WriteFile(name, content.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	store, _, _ := newTestServer(t)
	h := New(store, "test", nil, catalog).Handler()
	token := login(t, h)
	getTag := func() string {
		t.Helper()
		w := request(h, "GET", "/Items/movie", "", "", token)
		expectStatus(t, w, 200)
		var dto struct {
			ImageTags               map[string]string
			PrimaryImageAspectRatio float64
		}
		if err := json.Unmarshal(w.Body.Bytes(), &dto); err != nil {
			t.Fatal(err)
		}
		if dto.PrimaryImageAspectRatio != 2.0/3.0 || dto.ImageTags["Primary"] == "" {
			t.Fatal("image not advertised")
		}
		return dto.ImageTags["Primary"]
	}
	tag := getTag()
	imageURL := "/emby/Items/movie/Images/Primary?tag=" + url.QueryEscape(tag) + "&maxWidth=160&quality=90"
	w := request(h, "GET", imageURL, "", "", "")
	expectStatus(t, w, 200)
	if w.Header().Get("Content-Type") != "image/png" || !bytes.Equal(w.Body.Bytes(), content.Bytes()) || w.Header().Get("Cache-Control") != "private, no-cache, no-transform" {
		t.Fatal("incorrect image response")
	}
	etag := w.Header().Get("ETag")
	r := httptest.NewRequest("GET", imageURL, nil)
	r.Header.Set("If-None-Match", etag)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	expectStatus(t, w, 304)
	expectStatus(t, request(h, "HEAD", imageURL, "", "", ""), 200)
	expectStatus(t, request(h, "GET", "/Items/movie/Images/Primary/0", "", "", token), 200)
	expectStatus(t, request(h, "GET", "/Items/movie/Images/Primary", "", "", ""), 401)
	expectStatus(t, request(h, "GET", "/Items/other/Images/Primary?tag="+tag, "", "", ""), 401)
	expectStatus(t, request(h, "GET", imageURL+"&UserId=other", "", "", ""), 403)
	expectStatus(t, request(h, "GET", imageURL+"&TAG=wrong", "", "", ""), 400)
	expectStatus(t, request(h, "GET", imageURL, "", "", "bad-token"), 401)
	expectStatus(t, request(h, "GET", "/Items/movie", "", "", tag), 401)
	expectStatus(t, request(h, "GET", "/Items/other/Images/Primary", "", "", token), 404)
	changed := time.Now().Add(time.Second)
	if err := os.Chtimes(name, changed, changed); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, request(h, "GET", imageURL, "", "", ""), 404)
	newTag := getTag()
	if newTag == tag {
		t.Fatal("stale ImageTag")
	}
	imageURL = "/Items/movie/Images/Primary?tag=" + newTag
	expectStatus(t, request(h, "POST", "/Sessions/Logout", "", "", token), 204)
	r = httptest.NewRequest("GET", imageURL, nil)
	r.Header.Set("If-None-Match", etag)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	expectStatus(t, w, 401)
}
