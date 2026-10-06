package server

import (
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/TokenCemetery/coach/internal/state"
)

func (s *Server) imageRoutes(mux *http.ServeMux) {
	for _, route := range []string{"/items/{item}/images/{kind}", "/items/{item}/images/{kind}/{index}"} {
		mux.HandleFunc("GET "+route, s.itemImage)
	}
}

// imageAccess authorizes an image request by API token or, because browsers
// load images without headers, by an ImageTag signed for subject. The returned
// revision is the one the tag grants; it is empty for token access.
func (s *Server) imageAccess(w http.ResponseWriter, r *http.Request, subject string) (state.Session, string, bool) {
	token, err := requestToken(r)
	if err != nil {
		fail(w, 400, "InvalidAuthentication")
		return state.Session{}, "", false
	}
	revision := ""
	session, authErr := s.store.Authenticate(token)
	if token == "" {
		// ImageTags are opaque to Emby Web; it sends them as the tag query.
		query, err := catalogQuery(r)
		if err != nil {
			fail(w, 400, "InvalidQuery")
			return state.Session{}, "", false
		}
		session, revision, authErr = s.store.AuthenticateImageTag(subject, query["tag"])
	}
	if authErr != nil {
		fail(w, 401, "Unauthorized")
		return state.Session{}, "", false
	}
	for _, id := range values(r, "UserId") {
		if id != "" && !strings.EqualFold(id, session.UserID) {
			fail(w, 403, "Forbidden")
			return state.Session{}, "", false
		}
	}
	if !primaryImage(r) {
		fail(w, 404, "NotFound")
		return state.Session{}, "", false
	}
	return session, revision, true
}

// primaryImage reports whether the route names the only image Coach has.
func primaryImage(r *http.Request) bool {
	return r.PathValue("kind") == "primary" && (r.PathValue("index") == "" || r.PathValue("index") == "0")
}

// serveImage sends a validated JPEG/PNG with private revalidated caching.
func serveImage(w http.ResponseWriter, r *http.Request, file *os.File, mime, revision string) {
	w.Header().Set("Content-Type", mime)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, no-cache, no-transform")
	w.Header().Set("ETag", `W/"`+revision+`"`)
	// No second-resolution Last-Modified: mtime nanoseconds are in the ETag.
	http.ServeContent(w, r, "", time.Time{}, file)
}

func (s *Server) itemImage(w http.ResponseWriter, r *http.Request) {
	_, revision, ok := s.imageAccess(w, r, r.PathValue("item"))
	if !ok {
		return
	}
	item, found := s.findItem(r.PathValue("item"))
	if !found {
		fail(w, 404, "NotFound")
		return
	}
	file, info, err := s.catalog().OpenImage(item)
	if err != nil {
		fail(w, 404, "ImageUnavailable")
		return
	}
	defer func() { _ = file.Close() }()
	if revision != "" && revision != info.Revision {
		fail(w, 404, "ImageChanged")
		return
	}
	serveImage(w, r, file, info.MIME, info.Revision)
}
