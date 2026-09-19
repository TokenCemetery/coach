package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/TokenCemetery/coach/internal/state"
)

const testPassword = "test-only-password-not-a-real-secret"

func newTestServer(t *testing.T) (*state.Store, http.Handler, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Initialize("viewer", testPassword); err != nil {
		t.Fatal(err)
	}
	return s, New(s, "Coach test", nil, nil).Handler(), dir
}

func request(h http.Handler, method, path, contentType, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		r.Header.Set("X-Emby-Token", token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func expectStatus(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d", w.Code, status)
	}
}

func login(t *testing.T, h http.Handler) string {
	t.Helper()
	w := request(h, "POST", "/emby/Users/authenticatebyname?X-Emby-Client=Emby+Web&X-Emby-Device-Id=test-device", "application/x-www-form-urlencoded", url.Values{"Username": {"viewer"}, "Pw": {testPassword}}.Encode(), "")
	expectStatus(t, w, 200)
	var result struct {
		AccessToken string
		SessionInfo struct{ DeviceId string }
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.AccessToken == "" || result.SessionInfo.DeviceId != "test-device" {
		t.Fatal("missing token or client metadata")
	}
	return result.AccessToken
}

func TestEmbyLoginSettingsRestartLogout(t *testing.T) {
	s, h, dir := newTestServer(t)
	initial := s.Snapshot()
	for _, path := range []string{"/System/Info/Public", "/emby/system/info/public", "/EMBY/SYSTEM/INFO/PUBLIC"} {
		w := request(h, "GET", path, "", "", "")
		expectStatus(t, w, 200)
		var info map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil {
			t.Fatal(err)
		}
		if info["Version"] != CompatibilityVersion || info["Id"] != initial.ServerID {
			t.Fatal("incorrect public info")
		}
		for _, k := range []string{"LocalAddresses", "RemoteAddresses"} {
			if v, ok := info[k].([]any); !ok || len(v) != 0 {
				t.Fatal("address list must be []")
			}
		}
	}
	pub := request(h, "GET", "/Users/Public", "", "", "")
	expectStatus(t, pub, 200)
	for _, sensitive := range []string{"Salt", "PasswordHash", "Settings", "Policy"} {
		if strings.Contains(pub.Body.String(), sensitive) {
			t.Fatal("public user leaks private fields")
		}
	}
	expectStatus(t, request(h, "POST", "/Users/AuthenticateByName", "application/json", `{"Username":"viewer","Pw":"wrong"}`, ""), 401)
	token := login(t, h)
	uid := initial.User.ID
	for _, path := range []string{"/Users/" + uid, "/Users/Me", "/System/Info", "/Users/" + uid + "/Views", "/usersettings/" + uid} {
		expectStatus(t, request(h, "GET", path, "", "", ""), 401)
		expectStatus(t, request(h, "GET", path, "", "", token), 200)
	}
	views := request(h, "GET", "/Users/"+uid+"/Views", "", "", token)
	var page struct {
		Items            []any
		TotalRecordCount int
	}
	if err := json.Unmarshal(views.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Items == nil || len(page.Items) != 0 || page.TotalRecordCount != 0 {
		t.Fatal("incorrect empty library")
	}
	base := "/emby/usersettings/" + uid
	expectStatus(t, request(h, "POST", base+"?reqformat=json", "text/plain", `{"theme":"dark","old":true}`, token), 204)
	expectStatus(t, request(h, "POST", base+"/Partial?reqformat=json", "text/plain", `{"theme":"light"}`, token), 204)
	expectStatus(t, request(h, "POST", "/Sessions/Capabilities/Full?reqformat=json", "text/plain", `{"PlayableMediaTypes":["Video"],"SupportedCommands":[]}`, token), 204)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	h = New(reopened, "Coach test", nil, nil).Handler()
	if reopened.Snapshot().ServerID != initial.ServerID || reopened.Snapshot().User.ID != uid {
		t.Fatal("identity changed on restart")
	}
	expectStatus(t, request(h, "GET", "/Users/"+uid, "", "", token), 200)
	w := request(h, "GET", base, "", "", token)
	expectStatus(t, w, 200)
	var settings map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &settings); err != nil {
		t.Fatal(err)
	}
	if settings["theme"] != "light" || settings["old"] != true {
		t.Fatal("settings not persisted/merged")
	}
	expectStatus(t, request(h, "POST", "/Sessions/Logout", "", "", token), 204)
	expectStatus(t, request(h, "GET", base, "", "", token), 401)
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	final, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer final.Close()
	if _, err := final.Authenticate(token); err == nil {
		t.Fatal("logout did not survive restart")
	}
}

func TestTokenFormsAndAccessIsolation(t *testing.T) {
	s, h, _ := newTestServer(t)
	token := login(t, h)
	for _, query := range []string{"api_key", "X-Emby-Token", "x-emby-token", "X-MediaBrowser-Token"} {
		expectStatus(t, request(h, "GET", "/Users/Me?"+query+"="+token, "", "", ""), 200)
	}
	for _, header := range []string{"Authorization", "X-Emby-Authorization"} {
		for _, scheme := range []string{"Emby", "MediaBrowser"} {
			r := httptest.NewRequest("GET", "/Users/Me", nil)
			r.Header.Set(header, fmt.Sprintf(`%s Client="Web, Test", Token="%s"`, scheme, token))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			expectStatus(t, w, 200)
		}
	}
	for _, path := range []string{"/Users/other", "/usersettings/other", "/Users/other/Views", "/Items?UserId=other", "/Items?UserId=" + s.Snapshot().User.ID + "&userid=other"} {
		expectStatus(t, request(h, "GET", path, "", "", token), 403)
	}
	expectStatus(t, request(h, "GET", "/Users/Me?api_key=different", "", "", token), 400)
	expectStatus(t, request(h, "GET", "/Users/Me?X-Emby-Token="+token+"&X-Emby-Token=different", "", "", ""), 400)
	expectStatus(t, request(h, "POST", "/usersettings/other", "application/json", `{}`, token), 403)
	expectStatus(t, request(h, "POST", "/System/Restart", "", "", token), 404)
	expectStatus(t, request(h, "GET", "/Items/missing", "", "", token), 404)
}

func TestMalformedRequestsAndLimits(t *testing.T) {
	s, h, _ := newTestServer(t)
	token := login(t, h)
	path := "/usersettings/" + s.Snapshot().User.ID
	for _, body := range []string{"null", "[]", "{", "{} {}", strings.Repeat("x", 70<<10)} {
		expectStatus(t, request(h, "POST", path, "application/json", body, token), 400)
	}
	expectStatus(t, request(h, "POST", "/Users/AuthenticateByName", "text/plain", `{"Username":"viewer","Pw":"whatever"}`, ""), 415)
	expectStatus(t, request(h, "POST", "/Users/"+s.Snapshot().User.ID+"/Configuration/Partial", "application/json", `{"LatestItemsExcludes":null}`, token), 400)
	expectStatus(t, request(h, "OPTIONS", "/Users/AuthenticateByName", "", "", ""), 204)
	// Cheap malformed attempts still count toward the bounded login window.
	for range 20 {
		request(h, "POST", "/Users/AuthenticateByName", "application/json", "{", "")
	}
	w := request(h, "POST", "/Users/AuthenticateByName", "application/json", "{", "")
	expectStatus(t, w, 429)
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("missing retry interval")
	}
}
