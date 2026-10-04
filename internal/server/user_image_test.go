package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
)

func testPNG(t *testing.T, side int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewGray(image.Rect(0, 0, side, side))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func userImageTagOf(t *testing.T, h http.Handler, base, token string) string {
	t.Helper()
	w := request(h, "GET", base, "", "", token)
	expectStatus(t, w, 200)
	var user struct{ PrimaryImageTag string }
	if err := json.Unmarshal(w.Body.Bytes(), &user); err != nil {
		t.Fatal(err)
	}
	return user.PrimaryImageTag
}

func userImageFiles(t *testing.T, dir string) int {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "user-image-*"))
	if err != nil {
		t.Fatal(err)
	}
	return len(files)
}

func TestUserImageLifecycle(t *testing.T) {
	store, h, dir := newTestServer(t)
	token := login(t, h)
	base := "/Users/" + store.Snapshot().User.ID
	if userImageTagOf(t, h, base, token) != "" {
		t.Fatal("PrimaryImageTag without an avatar")
	}
	expectStatus(t, request(h, "GET", base+"/Images/Primary", "", "", token), 404)

	// Emby Web posts the file as base64 with a Content-Type from its extension.
	first := testPNG(t, 2)
	expectStatus(t, request(h, "POST", base+"/Images/Primary", "image/png", base64.StdEncoding.EncodeToString(first), token), 204)
	tag := userImageTagOf(t, h, base, token)
	if tag == "" {
		t.Fatal("no PrimaryImageTag after upload")
	}
	// Browsers load the avatar with the tag only, without a token.
	w := request(h, "GET", base+"/Images/Primary?height=200&tag="+url.QueryEscape(tag), "", "", "")
	expectStatus(t, w, 200)
	if !bytes.Equal(w.Body.Bytes(), first) || w.Header().Get("Content-Type") != "image/png" {
		t.Fatal("avatar differs from the upload")
	}
	expectStatus(t, request(h, "HEAD", base+"/Images/Primary/0", "", "", token), 200)
	// An avatar tag is not valid for an item with the user's ID.
	expectStatus(t, request(h, "GET", "/Items/"+store.Snapshot().User.ID+"/Images/Primary?tag="+url.QueryEscape(tag), "", "", ""), 401)

	second := testPNG(t, 3)
	expectStatus(t, request(h, "POST", base+"/Images/Primary", "image/jpg", base64.StdEncoding.EncodeToString(second), token), 204)
	expectStatus(t, request(h, "GET", base+"/Images/Primary?tag="+url.QueryEscape(tag), "", "", ""), 404)
	if userImageFiles(t, dir) != 1 {
		t.Fatal("replaced avatar file was kept")
	}

	expectStatus(t, request(h, "POST", base+"/Images/Primary/Delete", "", "", token), 204)
	expectStatus(t, request(h, "GET", base+"/Images/Primary", "", "", token), 404)
	if userImageTagOf(t, h, base, token) != "" || userImageFiles(t, dir) != 0 {
		t.Fatal("avatar remains after delete")
	}
	expectStatus(t, request(h, "DELETE", base+"/Images/Primary", "", "", token), 204)
}

func TestUserImageRejectsInvalidUploads(t *testing.T) {
	store, h, dir := newTestServer(t)
	token := login(t, h)
	base := "/Users/" + store.Snapshot().User.ID
	valid := base64.StdEncoding.EncodeToString(testPNG(t, 2))
	for _, tc := range []struct {
		path, body string
		status     int
	}{
		{base + "/Images/Primary", "not base64!", 400},
		{base + "/Images/Primary", base64.StdEncoding.EncodeToString([]byte("plain text")), 400},
		{base + "/Images/Primary", "", 400},
		{base + "/Images/Backdrop", valid, 404},
		{base + "/Images/Primary/1", valid, 404},
		{"/Users/other/Images/Primary", valid, 403},
	} {
		w := request(h, "POST", tc.path, "image/png", tc.body, token)
		if w.Code != tc.status {
			t.Fatalf("%s %q: status %d, want %d", tc.path, tc.body, w.Code, tc.status)
		}
	}
	if userImageFiles(t, dir) != 0 || store.Snapshot().User.Image != nil {
		t.Fatal("rejected upload was stored")
	}
}
