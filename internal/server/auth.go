// Package server implements the Emby-compatible HTTP API, WebSocket events, and
// Emby Web asset serving.
package server

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TokenCemetery/coach/internal/state"
)

var authParameter = regexp.MustCompile(`([A-Za-z][A-Za-z0-9_-]*)="([^"\r\n]*)"`)

// Request values are case-insensitive in the observed Emby client contract.
func values(r *http.Request, key string) []string {
	var result []string
	for k, vv := range r.URL.Query() {
		if strings.EqualFold(k, key) {
			result = append(result, vv...)
		}
	}
	result = append(result, r.Header.Values(key)...)
	return result
}

func value(r *http.Request, key string) string {
	v := values(r, key)
	if len(v) > 0 {
		return v[0]
	}
	return ""
}

func authorization(r *http.Request) map[string][]string {
	result := map[string][]string{}
	for _, header := range []string{"Authorization", "X-Emby-Authorization"} {
		for _, v := range r.Header.Values(header) {
			scheme, rest, ok := strings.Cut(v, " ")
			if !ok || (!strings.EqualFold(scheme, "Emby") && !strings.EqualFold(scheme, "MediaBrowser")) {
				continue
			}
			for _, pair := range authParameter.FindAllStringSubmatch(rest, -1) {
				k := strings.ToLower(pair[1])
				result[k] = append(result[k], pair[2])
			}
		}
	}
	return result
}

func requestToken(r *http.Request) (string, error) {
	candidates := authorization(r)["token"]
	for _, key := range []string{"X-Emby-Token", "X-MediaBrowser-Token", "api_key"} {
		candidates = append(candidates, values(r, key)...)
	}
	var token string
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if len(candidate) > 512 || (token != "" && token != candidate) {
			return "", errors.New("conflicting or invalid authentication")
		}
		token = candidate
	}
	return token, nil
}

func clientInfo(r *http.Request) state.Session {
	auth := authorization(r)
	get := func(header, legacy string) string {
		v := value(r, header)
		if v == "" && len(auth[legacy]) > 0 {
			v = auth[legacy][0]
		}
		if len(v) > 256 {
			v = v[:256]
		}
		return v
	}
	return state.Session{Client: get("X-Emby-Client", "client"), DeviceID: get("X-Emby-Device-Id", "deviceid"), DeviceName: get("X-Emby-Device-Name", "device"), Version: get("X-Emby-Client-Version", "version")}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	d := json.NewDecoder(r.Body)
	if err := d.Decode(dst); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("expected one JSON value")
	}
	return nil
}

// readFields reads the string fields Emby clients post as a form or as JSON
// (Emby Web sends JSON as text/plain with reqformat=json). Names are returned
// in lower case; booleans become "true"/"false" and other JSON values are
// ignored. On failure the response has been written.
func readFields(w http.ResponseWriter, r *http.Request) (map[string]string, bool) {
	fields := map[string]string{}
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch {
	case ct == "application/x-www-form-urlencoded":
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		if r.ParseForm() != nil {
			fail(w, 400, "InvalidRequest")
			return nil, false
		}
		for k, v := range r.PostForm {
			if len(v) != 1 {
				fail(w, 400, "InvalidRequest")
				return nil, false
			}
			fields[strings.ToLower(k)] = v[0]
		}
	case ct == "application/json" || (ct == "text/plain" && strings.EqualFold(value(r, "reqformat"), "json")):
		var body map[string]any
		if decodeJSON(w, r, &body) != nil {
			fail(w, 400, "InvalidRequest")
			return nil, false
		}
		for k, v := range body {
			switch v := v.(type) {
			case string:
				fields[strings.ToLower(k)] = v
			case bool:
				fields[strings.ToLower(k)] = strconv.FormatBool(v)
			}
		}
	default:
		fail(w, 415, "UnsupportedMediaType")
		return nil, false
	}
	return fields, true
}

// verifying applies the login limit to any request that checks a password. It
// returns false after writing 429; otherwise release must be called.
func (s *Server) verifying(w http.ResponseWriter) (release func(), ok bool) {
	if !s.logins.allow() {
		w.Header().Set("Retry-After", "60")
		fail(w, 429, "TooManyRequests")
		return nil, false
	}
	select {
	case s.logins.active <- struct{}{}:
		return func() { <-s.logins.active }, true
	default:
		w.Header().Set("Retry-After", "1")
		fail(w, 429, "TooManyRequests")
		return nil, false
	}
}

// changePassword serves the Profile page form, which posts CurrentPw and NewPw.
// The administrator reset (ResetPassword without the current password) is
// refused: Coach's single user is not an administrator.
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request, token string, session state.Session) {
	release, ok := s.verifying(w)
	if !ok {
		return
	}
	defer release()
	fields, ok := readFields(w, r)
	if !ok {
		return
	}
	if fields["resetpassword"] == "true" {
		fail(w, 403, "Forbidden")
		return
	}
	current, next := fields["currentpw"], fields["newpw"]
	if len(current) > 1024 {
		fail(w, 400, "InvalidRequest")
		return
	}
	// Emby Web hides the current-password field for servers from 4.8.0.38 and
	// sends it empty. Coach still requires it (#46), and answers in plain text,
	// which the client's error dialog shows as written.
	if current == "" {
		failText(w, 400, "Coach requires the current password to change it, and this client did not send one.")
		return
	}
	revoked, err := s.store.ChangePassword(token, current, next)
	switch {
	case errors.Is(err, state.ErrPassword):
		failText(w, 400, "The new password must contain 12 to 1024 bytes.")
		return
	case errors.Is(err, state.ErrCredentials):
		failText(w, 401, "The current password is incorrect.")
		return
	case err != nil:
		s.changed(w, err)
		return
	}
	for _, id := range revoked {
		s.disconnectSession(id)
	}
	w.WriteHeader(http.StatusNoContent)
}

// A bounded global window and semaphore cap password verification work. This
// intentionally simple M1 limit does not retain attacker-controlled IP keys.
type loginLimit struct {
	mu     sync.Mutex
	start  time.Time
	count  int
	active chan struct{}
}

func (l *loginLimit) allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if time.Since(l.start) >= time.Minute {
		l.start, l.count = time.Now(), 0
	}
	if l.count >= 20 {
		return false
	}
	l.count++
	return true
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	release, ok := s.verifying(w)
	if !ok {
		return
	}
	defer release()
	fields, ok := readFields(w, r)
	if !ok {
		return
	}
	username, pw := fields["username"], fields["pw"]
	if username == "" || len(username) > 128 || len(pw) > 1024 {
		fail(w, 400, "InvalidRequest")
		return
	}
	token, session, err := s.store.Login(username, pw, clientInfo(r))
	if errors.Is(err, state.ErrCredentials) {
		fail(w, 401, "Unauthorized")
		return
	}
	if errors.Is(err, state.ErrSessionLimit) {
		fail(w, 429, "SessionLimitReached")
		return
	}
	if err != nil {
		s.internalError(w)
		return
	}
	d := s.store.Snapshot()
	respond(w, 200, object{"User": s.userDTO(d, token), "SessionInfo": sessionDTO(d, session), "AccessToken": token, "ServerId": d.ServerID})
}

type authenticated func(http.ResponseWriter, *http.Request, string, state.Session)

func (s *Server) protect(next authenticated) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, err := requestToken(r)
		if err != nil {
			fail(w, 400, "InvalidAuthentication")
			return
		}
		session, err := s.store.Authenticate(token)
		if err != nil {
			fail(w, 401, "Unauthorized")
			return
		}
		ids := append(values(r, "UserId"), r.PathValue("user"))
		for _, id := range ids {
			if id != "" && !strings.EqualFold(id, session.UserID) {
				fail(w, 403, "Forbidden")
				return
			}
		}
		next(w, r, token, session)
	}
}
