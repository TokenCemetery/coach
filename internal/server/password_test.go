package server

import (
	"net/url"
	"testing"
)

func TestChangePassword(t *testing.T) {
	store, h, _ := newTestServer(t)
	token, other := login(t, h), login(t, h)
	path := "/Users/" + store.Snapshot().User.ID + "/Password"
	const next = "another-test-only-password"
	form := func(values url.Values) string { return values.Encode() }
	for _, tc := range []struct {
		path, body string
		status     int
	}{
		{path, form(url.Values{"CurrentPw": {"wrong-test-password"}, "NewPw": {next}}), 401},
		{path, form(url.Values{"CurrentPw": {testPassword}, "NewPw": {"short"}}), 400},
		{path, form(url.Values{"ResetPassword": {"true"}}), 403},
		{"/Users/other/Password", form(url.Values{"CurrentPw": {testPassword}, "NewPw": {next}}), 403},
	} {
		w := request(h, "POST", tc.path, "application/x-www-form-urlencoded; charset=UTF-8", tc.body, token)
		if w.Code != tc.status {
			t.Fatalf("%s %s: status %d, want %d", tc.path, tc.body, w.Code, tc.status)
		}
	}
	// Emby Web posts the Profile form as application/x-www-form-urlencoded.
	expectStatus(t, request(h, "POST", path, "application/x-www-form-urlencoded; charset=UTF-8", form(url.Values{"CurrentPw": {testPassword}, "NewPw": {next}}), token), 204)
	expectStatus(t, request(h, "GET", "/Users/me", "", "", token), 200)
	expectStatus(t, request(h, "GET", "/Users/me", "", "", other), 401)
	signIn := func(pw string) int {
		return request(h, "POST", "/Users/authenticatebyname", "application/x-www-form-urlencoded", form(url.Values{"Username": {"viewer"}, "Pw": {pw}}), "").Code
	}
	if signIn(testPassword) != 401 || signIn(next) != 200 {
		t.Fatal("login does not use the new password")
	}
	// JSON as Emby Web sends it with reqformat=json.
	expectStatus(t, request(h, "POST", path+"?reqformat=json", "text/plain", `{"CurrentPw":"`+next+`","NewPw":"`+testPassword+`"}`, token), 204)
}
