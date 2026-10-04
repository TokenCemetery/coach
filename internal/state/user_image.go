package state

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
)

// UserImage describes the stored avatar. The file is named by its revision, a
// content hash, so a reader never sees a partly replaced image.
type UserImage struct {
	Revision string
	MIME     string
}

func (s *Store) userImagePath(revision string) string {
	return filepath.Join(filepath.Dir(s.path), "user-image-"+revision)
}

// SetUserImage stores an already validated avatar. The file is written and
// synced before the state refers to it; the replaced file is removed after.
func (s *Store) SetUserImage(token string, content []byte, mime string) error {
	sum := sha256.Sum256(content)
	image := UserImage{Revision: hex.EncodeToString(sum[:16]), MIME: mime}
	name := s.userImagePath(image.Revision)
	if err := writeFileSynced(name, content); err != nil {
		return err
	}
	var previous *UserImage
	err := s.Change(token, func(d *Data, _ *Session) {
		previous = d.User.Image
		d.User.Image = &image
	})
	if err != nil {
		if previous == nil || previous.Revision != image.Revision {
			_ = os.Remove(name)
		}
		return err
	}
	if previous != nil && previous.Revision != image.Revision {
		_ = os.Remove(s.userImagePath(previous.Revision))
	}
	return nil
}

// DeleteUserImage removes the avatar. Deleting a missing avatar succeeds.
func (s *Store) DeleteUserImage(token string) error {
	var previous *UserImage
	if err := s.Change(token, func(d *Data, _ *Session) {
		previous = d.User.Image
		d.User.Image = nil
	}); err != nil {
		return err
	}
	if previous != nil {
		_ = os.Remove(s.userImagePath(previous.Revision))
	}
	return nil
}

// OpenUserImage opens the current avatar file.
func (s *Store) OpenUserImage() (*os.File, UserImage, error) {
	s.mu.RLock()
	image := s.data.User.Image
	s.mu.RUnlock()
	if image == nil {
		return nil, UserImage{}, os.ErrNotExist
	}
	file, err := os.Open(s.userImagePath(image.Revision))
	return file, *image, err
}

func writeFileSynced(name string, content []byte) error {
	f, err := os.CreateTemp(filepath.Dir(name), ".user-image-*")
	if err != nil {
		return err
	}
	// After a successful rename this removal fails harmlessly.
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err = f.Write(content); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), name)
}
