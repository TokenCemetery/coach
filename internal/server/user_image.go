package server

import (
	"bytes"
	"encoding/base64"
	"image"
	_ "image/jpeg" // register JPEG for image.DecodeConfig
	_ "image/png"  // register PNG for image.DecodeConfig
	"io"
	"net/http"
	"strings"

	"github.com/TokenCemetery/coach/internal/state"
)

// Avatar limits match local covers: the file is stored and served as uploaded,
// and only its header is decoded.
const (
	maxUserImageBytes = 8 << 20
	maxUserImageSide  = 16384
	maxUserImageArea  = 32_000_000
)

// userImageSubject keeps avatar ImageTags from authorizing an item image.
func userImageSubject(userID string) string { return "user:" + strings.ToLower(userID) }

func (s *Server) userImageRoutes(mux *http.ServeMux) {
	for _, route := range []string{"/users/{user}/images/{kind}", "/users/{user}/images/{kind}/{index}"} {
		// GET also serves HEAD. It authenticates by ImageTag, so it cannot use protect.
		mux.HandleFunc("GET "+route, s.userImage)
		mux.HandleFunc("POST "+route, s.protect(s.uploadUserImage))
		mux.HandleFunc("DELETE "+route, s.protect(s.deleteUserImage))
		// Emby Web deletes with POST .../Delete.
		mux.HandleFunc("POST "+route+"/delete", s.protect(s.deleteUserImage))
	}
}

func (s *Server) userImage(w http.ResponseWriter, r *http.Request) {
	user := r.PathValue("user")
	_, revision, ok := s.imageAccess(w, r, userImageSubject(user))
	if !ok {
		return
	}
	if !strings.EqualFold(user, s.store.Snapshot().User.ID) {
		fail(w, 404, "NotFound")
		return
	}
	file, info, err := s.store.OpenUserImage()
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

// uploadUserImage accepts the body Emby Web sends: the file as base64 text,
// with a Content-Type derived from the file extension. The format is taken
// from the decoded header, not from the Content-Type.
func (s *Server) uploadUserImage(w http.ResponseWriter, r *http.Request, token string, session state.Session) {
	if !primaryImage(r) {
		fail(w, 404, "NotFound")
		return
	}
	limit := base64.StdEncoding.EncodedLen(maxUserImageBytes)
	encoded, err := io.ReadAll(io.LimitReader(r.Body, int64(limit)+1))
	if err != nil || len(encoded) > limit {
		fail(w, 413, "ImageTooLarge")
		return
	}
	content, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(encoded)))
	if err != nil || len(content) == 0 || len(content) > maxUserImageBytes {
		fail(w, 400, "InvalidImage")
		return
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(content))
	if err != nil || (format != "jpeg" && format != "png") || config.Width <= 0 || config.Height <= 0 ||
		config.Width > maxUserImageSide || config.Height > maxUserImageSide || config.Width*config.Height > maxUserImageArea {
		fail(w, 400, "InvalidImage")
		return
	}
	s.changed(w, s.store.SetUserImage(token, content, "image/"+format))
}

func (s *Server) deleteUserImage(w http.ResponseWriter, r *http.Request, token string, session state.Session) {
	if !primaryImage(r) {
		fail(w, 404, "NotFound")
		return
	}
	s.changed(w, s.store.DeleteUserImage(token))
}

// userImageTag is the user's PrimaryImageTag for this session, or "" when
// there is no avatar.
func (s *Server) userImageTag(d state.Data, token string) string {
	if d.User.Image == nil {
		return ""
	}
	return s.store.ImageTag(token, userImageSubject(d.User.ID), d.User.Image.Revision)
}
