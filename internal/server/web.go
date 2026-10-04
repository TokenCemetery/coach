package server

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

const (
	maxUpstreamFetches = 16
	upstreamSlotWait   = 10 * time.Second
	maxUpstreamAsset   = 32 << 20
)

// WebAssets serves a user-supplied Emby installation's static files. API calls
// never pass through this handler. The client uses Coach's same-origin API.
func WebAssets(origin string) (http.Handler, error) {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("web upstream must be an http(s) origin without credentials, path, query or fragment")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	client := &http.Client{Transport: transport, Timeout: 20 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
	// Anonymous requests must not multiply load on the upstream Emby without
	// bound. Emby Web loads a few hundred files at start, so a request waits
	// briefly for a slot instead of failing at once: a refused script stops
	// the client.
	slots := make(chan struct{}, maxUpstreamFetches)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			fail(w, 405, "MethodNotAllowed")
			return
		}
		if !validAssetPath(r.URL.Path) {
			fail(w, 400, "InvalidAssetPath")
			return
		}
		if !allowedAsset(r.URL.Path) {
			fail(w, 404, "NotFound")
			return
		}
		target := *u
		target.Path = r.URL.Path
		// Only the asset version may be forwarded. In particular, no token,
		// cookie, Authorization, Referer, or user-controlled upstream URL.
		q := url.Values{}
		if v := r.URL.Query().Get("v"); v != "" && len(v) < 128 {
			q.Set("v", v)
		}
		target.RawQuery = q.Encode()
		req, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), nil)
		if err != nil {
			fail(w, 502, "AssetUnavailable")
			return
		}
		wait := time.NewTimer(upstreamSlotWait)
		select {
		case slots <- struct{}{}:
			wait.Stop()
			defer func() { <-slots }()
		case <-wait.C:
			w.Header().Set("Retry-After", "5")
			fail(w, 503, "TooManyConnections")
			return
		case <-r.Context().Done():
			wait.Stop()
			return
		}
		res, err := client.Do(req) //nolint:gosec // host is the operator-configured upstream; only validated asset paths and "v" are forwarded
		if err != nil {
			fail(w, 502, "AssetUnavailable")
			return
		}
		defer func() { _ = res.Body.Close() }()
		if res.StatusCode != http.StatusOK {
			if res.StatusCode == http.StatusNotFound {
				fail(w, 404, "AssetNotFound")
			} else {
				fail(w, 502, "AssetUnavailable")
			}
			return
		}
		if res.ContentLength > maxUpstreamAsset {
			fail(w, 502, "AssetUnavailable")
			return
		}
		for _, key := range []string{"Content-Type", "Content-Length", "ETag", "Last-Modified"} {
			if v := res.Header.Get(key); v != "" {
				w.Header().Set(key, v)
			}
		}
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			// Without Content-Length the body is cut at the cap; the client
			// then sees a truncated asset instead of an unbounded transfer.
			_, _ = io.Copy(w, io.LimitReader(res.Body, maxUpstreamAsset))
		}
	}), nil
}

func validAssetPath(name string) bool {
	if !strings.HasPrefix(name, "/web/") || path.Clean(name) != name || strings.ContainsAny(name, "\\%") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".") {
			return false
		}
	}
	return true
}

func allowedAsset(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".js", ".css", ".html", ".json", ".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".ico", ".woff", ".woff2", ".ttf", ".wasm", ".mp3":
		return true
	}
	return false
}
