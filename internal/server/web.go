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
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "HEAD" {
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
		res, err := client.Do(req)
		if err != nil {
			fail(w, 502, "AssetUnavailable")
			return
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			if res.StatusCode == 404 {
				fail(w, 404, "AssetNotFound")
			} else {
				fail(w, 502, "AssetUnavailable")
			}
			return
		}
		for _, key := range []string{"Content-Type", "Content-Length", "ETag", "Last-Modified"} {
			if v := res.Header.Get(key); v != "" {
				w.Header().Set(key, v)
			}
		}
		w.WriteHeader(200)
		if r.Method != "HEAD" {
			_, _ = io.Copy(w, res.Body)
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
