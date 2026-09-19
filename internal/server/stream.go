package server

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/TokenCemetery/coach/internal/media"
)

const streamWriteTimeout = 30 * time.Second

// streamWriter renews the write deadline per chunk, not per response. Embedding
// only ResponseWriter prevents io.Copy from bypassing Write through ReaderFrom.
type streamWriter struct {
	http.ResponseWriter
	controller *http.ResponseController
}

func (w *streamWriter) refreshDeadline() error {
	err := w.controller.SetWriteDeadline(time.Now().Add(streamWriteTimeout))
	if errors.Is(err, http.ErrNotSupported) {
		// In-memory response recorders have no connection deadline.
		return nil
	}
	return err
}

func (w *streamWriter) Write(p []byte) (int, error) {
	if err := w.refreshDeadline(); err != nil {
		return 0, err
	}
	return w.ResponseWriter.Write(p)
}

func serveMediaFile(w http.ResponseWriter, r *http.Request, file *os.File, item media.Item) {
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		fail(w, 404, "MediaUnavailable")
		return
	}
	stream := &streamWriter{ResponseWriter: w, controller: http.NewResponseController(w)}
	if err := stream.refreshDeadline(); err != nil {
		fail(w, 500, "StreamUnavailable")
		return
	}
	if mime := containerMIME(item.Container); mime != "" {
		w.Header().Set("Content-Type", mime)
	}
	w.Header().Set("Cache-Control", "private, no-cache, no-transform")
	// File metadata is not a content hash: use a weak validator, which cannot
	// authorize If-Range reuse. Read current metadata, not the startup snapshot.
	w.Header().Set("ETag", fmt.Sprintf(`W/"%s-%x-%x"`, derive("stream", item.ID), info.Size(), info.ModTime().UnixNano()))
	http.ServeContent(stream, r, "", info.ModTime(), file)
}
