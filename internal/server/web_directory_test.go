package server

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestLocalWebAssets(t *testing.T) {
	dir := t.TempDir()
	index := "<!doctype html><html><head></head><body>test</body></html>"
	for name, content := range map[string]string{"index.html": index, "app.js": "test-script", "secret.txt": "not an asset", ".hidden.json": "private"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "directory.js"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe.js"), 0600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.js")
	if err := os.WriteFile(outside, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escape.js")); err != nil {
		t.Fatal(err)
	}
	web, err := OpenWebDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer web.Close()
	w := request(web, "GET", "/web/index.html", "", "", "")
	expectStatus(t, w, 200)
	if !strings.Contains(w.Body.String(), `data-appversion="`+CompatibilityVersion+`"`) || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Fatal("index bootstrap metadata or MIME missing")
	}
	unchanged, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil || string(unchanged) != index {
		t.Fatal("UI index was modified on disk")
	}
	w = request(web, "GET", "/web/app.js?v=test&api_key=ignored", "", "", "")
	expectStatus(t, w, 200)
	if w.Body.String() != "test-script" || !strings.Contains(w.Header().Get("Content-Type"), "javascript") {
		t.Fatal("incorrect JS body or MIME")
	}
	w = request(web, "HEAD", "/web/app.js", "", "", "")
	expectStatus(t, w, 200)
	if w.Body.Len() != 0 || w.Header().Get("Content-Length") != "11" {
		t.Fatal("incorrect HEAD response")
	}
	r := httptest.NewRequest("GET", "/web/app.js", nil)
	r.Header.Set("Range", "bytes=0-3")
	w = httptest.NewRecorder()
	web.ServeHTTP(w, r)
	expectStatus(t, w, 206)
	if w.Body.String() != "test" {
		t.Fatal("incorrect partial asset")
	}
	for _, name := range []string{"/web/../outside.js", "/web/%2e%2e/outside.js", "/web/%252e%252e/outside.js", "/web/foo%5c..%5capp.js", "/web/.hidden.json", "/web/secret.txt", "/web/directory.js", "/web/pipe.js", "/web/escape.js", "/web/missing.js", "/System/Info/Public"} {
		w := request(web, "GET", name, "", "", "")
		if w.Code < 400 || strings.Contains(w.Body.String(), "private") || strings.Contains(w.Body.String(), dir) {
			t.Fatalf("unsafe asset response: %s", name)
		}
	}
	expectStatus(t, request(web, "POST", "/web/app.js", "", "", ""), 405)
	store, _, _ := newTestServer(t)
	h := New(store, "test", web, nil).Handler()
	expectStatus(t, request(h, "GET", "/", "", "", ""), 302)
	expectStatus(t, request(h, "GET", "/System/Info/Public", "", "", ""), 200)
	expectStatus(t, request(h, "GET", "/web/app.js", "", "", ""), 200)
}

func TestLocalWebRequiresIndex(t *testing.T) {
	for _, contents := range []string{"", "<html data-appversion=\"other\"></html>", strings.Repeat("x", (1<<20)+1)} {
		dir := t.TempDir()
		if contents != "" {
			if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if web, err := OpenWebDirectory(dir); err == nil {
			_ = web.Close()
			t.Fatal("invalid web directory accepted")
		}
	}
}
