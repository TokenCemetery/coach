package state

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestImageAccessScopedRevokedAndDurable(t *testing.T) {
	store, dir := testStore(t)
	token, session, err := store.Login("viewer", "test-only-password", Session{})
	if err != nil {
		t.Fatal(err)
	}
	revision := strings.Repeat("a", 32)
	tag := store.ImageTag(token, "item", revision)
	if strings.Contains(tag, token) || strings.Contains(tag, tokenHash(token)) {
		t.Fatal("tag exposes credential")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	got, version, err := store.AuthenticateImageTag("item", tag)
	if err != nil || got.ID != session.ID || version != revision {
		t.Fatal("image grant lost on restart")
	}
	for _, test := range []struct{ item, tag string }{
		{"other", tag}, {"item", ""}, {"item", tag + "x"},
		{"item", strings.Repeat("b", 32) + tag[32:]},
	} {
		if _, _, err := store.AuthenticateImageTag(test.item, test.tag); !errors.Is(err, ErrSession) {
			t.Fatal("invalid grant accepted")
		}
	}
	if _, err := store.Authenticate(tag); !errors.Is(err, ErrSession) {
		t.Fatal("image grant used as API token")
	}
	if err := store.Logout(token); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AuthenticateImageTag("item", tag); !errors.Is(err, ErrSession) {
		t.Fatal("logout did not revoke image")
	}
	token, _, err = store.Login("viewer", "test-only-password", Session{})
	if err != nil {
		t.Fatal(err)
	}
	tag = store.ImageTag(token, "item", revision)
	if err := store.Change(token, func(_ *Data, session *Session) { session.ExpiresAt = time.Now().Add(-time.Second) }); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AuthenticateImageTag("item", tag); !errors.Is(err, ErrSession) {
		t.Fatal("expired grant accepted")
	}
	if store.ImageTag(token, "item", revision) != "" {
		t.Fatal("expired token signed a tag")
	}
}
