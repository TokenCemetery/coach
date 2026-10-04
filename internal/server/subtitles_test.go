package server

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TokenCemetery/coach/internal/media"
)

func TestSidecarSubtitleDelivery(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe required to open media root")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Movie.ru.srt"), []byte("1\r\n00:00:01,000 --> 00:00:02,000\r\nПривет\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	catalog, err := media.Scan(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = catalog.Close() }()
	movie := playbackMovie()
	movie.Path = "Movie.mp4"
	// A bitmap track cannot become WebVTT, so it stays undeliverable.
	movie.Streams[3].Codec = "hdmv_pgs_subtitle"
	movie.Streams = append(movie.Streams, media.Stream{Index: 4, Type: "Subtitle", Codec: "srt", Language: "ru", Path: "Movie.ru.srt"})
	catalog.Items = []media.Item{movie}
	store, _, _ := newTestServer(t)
	h := New(store, "test", nil, catalog).Handler()
	token := login(t, h)
	info := func(subtitle string) (sources []map[string]any, errorCode string) {
		t.Helper()
		w := request(h, "POST", "/Items/movie/PlaybackInfo", "application/json", `{"SubtitleStreamIndex":`+subtitle+`}`, token)
		expectStatus(t, w, 200)
		var response struct {
			MediaSources []map[string]any
			ErrorCode    string
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response.MediaSources, response.ErrorCode
	}
	sources, code := info("4")
	if code != "" || sources[0]["SupportsDirectStream"] != true || sources[0]["DefaultSubtitleStreamIndex"] != float64(4) {
		t.Fatalf("external subtitle: code %q, source %v", code, sources[0])
	}
	var external map[string]any
	for _, stream := range sources[0]["MediaStreams"].([]any) {
		if s := stream.(map[string]any); s["Index"] == float64(4) {
			external = s
		}
	}
	deliveryURL, _ := external["DeliveryUrl"].(string)
	if external["IsExternal"] != true || external["DeliveryMethod"] != "External" || external["DeliveryFormat"] != "vtt" || !strings.HasPrefix(deliveryURL, "/videos/movie/mediasource_movie/subtitles/4/stream.vtt?api_key=") {
		t.Fatalf("external stream: %v", external)
	}
	if _, code := info("3"); code != "NoCompatibleStream" {
		t.Fatalf("bitmap subtitle: code %q, want NoCompatibleStream", code)
	}
	// The player fetches the URL with only the token it carries.
	w := request(h, "GET", "/emby"+deliveryURL, "", "", "")
	expectStatus(t, w, 200)
	if w.Header().Get("Content-Type") != "text/vtt; charset=utf-8" || w.Body.String() != "WEBVTT\n\n1\n00:00:01.000 --> 00:00:02.000\nПривет\n\n" {
		t.Fatalf("subtitle: %q %q", w.Header().Get("Content-Type"), w.Body.String())
	}
	expectStatus(t, request(h, "GET", "/Videos/movie/mediasource_movie/Subtitles/4/0/Stream.vtt", "", "", token), 200)
	for _, path := range []string{"/Videos/movie/mediasource_movie/Subtitles/3/Stream.vtt", "/Videos/movie/mediasource_movie/Subtitles/4/Stream.srt", "/Videos/movie/other/Subtitles/4/Stream.vtt"} {
		expectStatus(t, request(h, "GET", path, "", "", token), 404)
	}
	expectStatus(t, request(h, "GET", "/Videos/movie/mediasource_movie/Subtitles/4/Stream.vtt", "", "", ""), 401)
}
