package server

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// WebDirectory serves only static files from an explicitly selected UI root.
type WebDirectory struct {
	root  *os.Root
	index []byte
}

func OpenWebDirectory(directory string) (*WebDirectory, error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, errors.New("cannot open web directory")
	}
	web := &WebDirectory{root: root}
	file, err := web.open("index.html")
	if err != nil {
		_ = root.Close()
		return nil, errors.New("web directory requires a readable index.html")
	}
	defer file.Close()
	const maxIndex = 1 << 20
	index, err := io.ReadAll(io.LimitReader(file, maxIndex+1))
	if err != nil || len(index) > maxIndex {
		_ = root.Close()
		return nil, errors.New("web index is unreadable or exceeds 1 MiB")
	}
	// The unprocessed Emby package has a bare <html> tag. Supply the server's
	// compatibility version in memory; never rewrite the user's UI files.
	if bytes.Contains(index, []byte("<html>")) {
		index = bytes.Replace(index, []byte("<html>"), []byte(`<html data-appversion="`+CompatibilityVersion+`">`), 1)
	} else if !bytes.Contains(index, []byte(`data-appversion="`+CompatibilityVersion+`"`)) {
		_ = root.Close()
		return nil, errors.New("web index must target Emby Web " + CompatibilityVersion)
	}
	web.index = index
	return web, nil
}

func (web *WebDirectory) Close() error { return web.root.Close() }

func (web *WebDirectory) open(name string) (*os.File, error) {
	file, err := web.root.OpenFile(filepath.FromSlash(name), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("asset unavailable")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, errors.New("asset unavailable")
	}
	return file, nil
}

func (web *WebDirectory) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
		fail(w, 404, "AssetNotFound")
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/web/")
	if name == "index.html" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(web.index))
		return
	}
	file, err := web.open(name)
	if err != nil {
		fail(w, 404, "AssetNotFound")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		fail(w, 404, "AssetNotFound")
		return
	}
	http.ServeContent(w, r, name, info.ModTime(), file)
}
