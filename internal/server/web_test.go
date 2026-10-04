package server

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWebAssetsDoNotForwardCredentialsOrAPI(t *testing.T) {
	var count int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		if r.Method != http.MethodGet || r.URL.Path != "/web/example.js" || r.URL.RawQuery != "v=4.10.0.40" {
			t.Error("unexpected upstream target")
		}
		for _, key := range []string{"Authorization", "X-Emby-Token", "Cookie", "Referer"} {
			if r.Header.Get(key) != "" {
				t.Errorf("forwarded %s", key)
			}
		}
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("Set-Cookie", "upstream=private")
		_, _ = w.Write([]byte("test asset"))
	}))
	defer upstream.Close()
	proxy, err := WebAssets(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/web/example.js?v=4.10.0.40&api_key=secret&X-Emby-Token=secret", nil)
	for _, key := range []string{"Authorization", "X-Emby-Token", "Cookie", "Referer"} {
		r.Header.Set(key, "test-secret")
	}
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, r)
	expectStatus(t, w, 200)
	if w.Body.String() != "test asset" || w.Header().Get("Set-Cookie") != "" {
		t.Fatal("incorrect asset or leaked cookie")
	}
	for _, path := range []string{"/Users/Public", "/web/../System/Info/Public", "/web/%2e%2e/System/Info/Public", "/web/foo%5c..%5capi.js", "/web/foo%252e.js", "/web/example"} {
		w := request(proxy, "GET", path, "", "", "")
		if w.Code < 400 {
			t.Fatal("invalid asset path accepted")
		}
	}
	expectStatus(t, request(proxy, "POST", "/web/example.js", "application/json", `{}`, ""), 405)
	if count != 1 {
		t.Fatal("unexpected upstream requests")
	}
}

func TestWebAssetsRejectRedirectsAndInvalidOrigins(t *testing.T) {
	for _, origin := range []string{"file:///tmp", "http://user:password@example.com", "http://example.com/api", "http://example.com?token=secret", "http://example.com/#x"} {
		if _, err := WebAssets(origin); err == nil {
			t.Fatal("unsafe origin accepted")
		}
	}
	targetCalls := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls++ }))
	defer target.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer upstream.Close()
	proxy, err := WebAssets(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	w := request(proxy, "GET", "/web/index.html", "", "", "")
	expectStatus(t, w, 502)
	if targetCalls != 0 || strings.Contains(w.Body.String(), target.URL) {
		t.Fatal("followed or leaked upstream redirect")
	}
}

func TestWebAssetsCapUpstreamFetches(t *testing.T) {
	var active, peak atomic.Int64
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/web/huge.js" {
			w.Header().Set("Content-Length", strconv.Itoa(maxUpstreamAsset+1))
			return
		}
		n := active.Add(1)
		defer active.Add(-1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		<-release
		_, _ = w.Write([]byte("asset"))
	}))
	defer upstream.Close()
	defer releaseAll() // runs before Close, so a failed check cannot hang it
	proxy, err := WebAssets(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	// Twice the cap arrives at once: the extra half waits, none fails.
	var wg sync.WaitGroup
	codes := make(chan int, 2*maxUpstreamFetches)
	for range 2 * maxUpstreamFetches {
		wg.Go(func() { codes <- request(proxy, "GET", "/web/slow.js", "", "", "").Code })
	}
	for active.Load() < maxUpstreamFetches {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if got := active.Load(); got != maxUpstreamFetches {
		t.Fatalf("%d concurrent upstream fetches, want %d", got, maxUpstreamFetches)
	}
	releaseAll()
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != 200 {
			t.Fatalf("queued request: status %d", code)
		}
	}
	if peak.Load() > maxUpstreamFetches {
		t.Fatalf("peak %d concurrent fetches", peak.Load())
	}
	expectStatus(t, request(proxy, "GET", "/web/huge.js", "", "", ""), 502)
}
