package server

import (
	"net/http"
	"strings"
	"time"
)

func (s *Server) imageRoutes(mux *http.ServeMux) {
	for _, route := range []string{"/items/{item}/images/{kind}", "/items/{item}/images/{kind}/{index}"} {
		mux.HandleFunc("GET "+route, s.itemImage)
	}
}

func (s *Server) itemImage(w http.ResponseWriter, r *http.Request) {
	token, err := requestToken(r)
	if err != nil {
		fail(w, 400, "InvalidAuthentication")
		return
	}
	revision := ""
	session, authErr := s.store.Authenticate(token)
	if token == "" {
		// ImageTags are opaque to Emby Web; it sends them as the tag query.
		query, err := catalogQuery(r)
		if err != nil {
			fail(w, 400, "InvalidQuery")
			return
		}
		session, revision, authErr = s.store.AuthenticateImageTag(r.PathValue("item"), query["tag"])
	}
	if authErr != nil {
		fail(w, 401, "Unauthorized")
		return
	}
	for _, id := range values(r, "UserId") {
		if id != "" && !strings.EqualFold(id, session.UserID) {
			fail(w, 403, "Forbidden")
			return
		}
	}
	if r.PathValue("kind") != "primary" || (r.PathValue("index") != "" && r.PathValue("index") != "0") {
		fail(w, 404, "NotFound")
		return
	}
	item, found := s.findItem(r.PathValue("item"))
	if !found {
		fail(w, 404, "NotFound")
		return
	}
	file, info, err := s.media.OpenImage(item)
	if err != nil {
		fail(w, 404, "ImageUnavailable")
		return
	}
	defer file.Close()
	if revision != "" && revision != info.Revision {
		fail(w, 404, "ImageChanged")
		return
	}
	w.Header().Set("Content-Type", info.MIME)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, no-cache, no-transform")
	w.Header().Set("ETag", `W/"`+info.Revision+`"`)
	// No second-resolution Last-Modified: mtime nanoseconds are in the ETag.
	http.ServeContent(w, r, "", time.Time{}, file)
}
