package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A failed change must not remove the file the committed state references,
// even when the upload has the same content as the current avatar.
func TestUserImageFailedChangeKeepsCommittedFile(t *testing.T) {
	s, dir := testStore(t)
	token, _, err := s.Login("viewer", "test-only-password", Session{})
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("not decoded by the store")
	if err := s.SetUserImage(token, content, "image/png"); err != nil {
		t.Fatal(err)
	}
	if err := s.Logout(token); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserImage(token, content, "image/png"); !errors.Is(err, ErrSession) {
		t.Fatalf("re-upload after logout: %v", err)
	}
	if err := s.SetUserImage(token, []byte("other"), "image/png"); !errors.Is(err, ErrSession) {
		t.Fatalf("new upload after logout: %v", err)
	}
	if err := s.DeleteUserImage(token); !errors.Is(err, ErrSession) {
		t.Fatalf("delete after logout: %v", err)
	}
	file, _, err := s.OpenUserImage()
	if err != nil {
		t.Fatal("committed avatar file was removed:", err)
	}
	_ = file.Close()
	files, _ := filepath.Glob(filepath.Join(dir, "user-image-*"))
	if len(files) != 1 {
		t.Fatalf("avatar files = %d, want 1 (rejected upload must be cleaned up)", len(files))
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json")); err != nil {
		t.Fatal(err)
	}
}
