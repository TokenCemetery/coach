package server

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"regexp"
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
	if !s.logins.allow() {
		w.Header().Set("Retry-After", "60")
		fail(w, 429, "TooManyRequests")
		return
	}
	select {
	case s.logins.active <- struct{}{}:
		defer func() { <-s.logins.active }()
	default:
		w.Header().Set("Retry-After", "1")
		fail(w, 429, "TooManyRequests")
		return
	}
	var body struct {
		Username string
		Pw       string
	}
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct == "application/x-www-form-urlencoded" {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		if r.ParseForm() != nil {
			fail(w, 400, "InvalidRequest")
			return
		}
		for k, v := range r.PostForm {
			if len(v) != 1 {
				fail(w, 400, "InvalidRequest")
				return
			}
			switch strings.ToLower(k) {
			case "username":
				body.Username = v[0]
			case "pw":
				body.Pw = v[0]
			}
		}
	} else if ct == "application/json" || (ct == "text/plain" && strings.EqualFold(value(r, "reqformat"), "json")) {
		if decodeJSON(w, r, &body) != nil {
			fail(w, 400, "InvalidRequest")
			return
		}
	} else {
		fail(w, 415, "UnsupportedMediaType")
		return
	}
	if body.Username == "" || len(body.Username) > 128 || len(body.Pw) > 1024 {
		fail(w, 400, "InvalidRequest")
		return
	}
	token, session, err := s.store.Login(body.Username, body.Pw, clientInfo(r))
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
	respond(w, 200, object{"User": userDTO(d), "SessionInfo": sessionDTO(d, session), "AccessToken": token, "ServerId": d.ServerID})
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
