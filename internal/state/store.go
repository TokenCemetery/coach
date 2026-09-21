// Package state persists the small authentication/settings state used in M1.
package state

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const passwordIterations = 600000
const maxStateBytes = 4 << 20

var (
	ErrCredentials  = errors.New("invalid credentials")
	ErrSession      = errors.New("invalid or expired session")
	ErrSessionLimit = errors.New("session limit reached")
	ErrStateLimit   = errors.New("state size limit reached")
)

type User struct {
	ID            string
	Name          string
	Salt          []byte
	PasswordHash  []byte
	Iterations    int
	Configuration map[string]json.RawMessage
	Settings      map[string]json.RawMessage
	// Items holds per-item playback state keyed by item ID. It is absent in
	// state written before playback existed and is created on first write.
	Items map[string]ItemState
}

// ItemState is the durable part of an item's UserData.
type ItemState struct {
	PositionTicks int64
	PlayCount     int
	Played        bool
	IsFavorite    bool
	LastPlayed    time.Time
}

// maxTrackedItems bounds the state file independently of its byte limit.
const maxTrackedItems = 20000

type Session struct {
	ID           string
	UserID       string
	Client       string
	DeviceID     string
	DeviceName   string
	Version      string
	ExpiresAt    time.Time
	Capabilities map[string]json.RawMessage
	RecentPlays  []PlaybackRecord `json:",omitempty"`
}

// PlaybackRecord retains a bounded retry window per authenticated client.
type PlaybackRecord struct {
	ID      string
	ItemID  string
	Stopped bool
}

type Data struct {
	SchemaVersion int
	ServerID      string
	User          User
	Sessions      map[string]Session
}

type Store struct {
	mu   sync.RWMutex
	path string
	lock *os.File
	data Data
}

func randomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Open locks a data directory for this process. Missing state requires explicit
// initialization; a corrupt or newer schema is never silently replaced.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "state.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("data directory is already in use: %w", err)
	}
	s := &Store{path: filepath.Join(dir, "state.json"), lock: lock}
	f, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	var b []byte
	if err == nil {
		b, err = io.ReadAll(io.LimitReader(f, maxStateBytes+1))
		_ = f.Close()
		if len(b) > maxStateBytes {
			err = ErrStateLimit
		}
	}
	if err == nil {
		err = json.Unmarshal(b, &s.data)
	}
	if err == nil && (s.data.SchemaVersion != 1 || s.data.ServerID == "" || s.data.User.ID == "" || s.data.User.Name == "" || len(s.data.User.Salt) != 16 || len(s.data.User.PasswordHash) != 32 || s.data.User.Iterations != passwordIterations || s.data.User.Settings == nil || s.data.User.Configuration == nil || s.data.Sessions == nil) {
		err = errors.New("invalid or unsupported state schema")
	}
	if err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.lock.Close() }

func (s *Store) Initialized() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.SchemaVersion != 0
}

func (s *Store) Snapshot() Data {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return clone(s.data)
}

func clone(d Data) Data {
	b, _ := json.Marshal(d)
	var copy Data
	_ = json.Unmarshal(b, &copy)
	return copy
}

// update commits to disk before publishing a snapshot to readers.
func (s *Store) update(fn func(*Data) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := clone(s.data)
	if err := fn(&next); err != nil {
		return err
	}
	b, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if len(b) > maxStateBytes {
		return ErrStateLimit
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), s.path); err != nil {
		return err
	}
	// Rename already committed: keep memory consistent even if directory fsync fails.
	s.data = next
	dir, err := os.Open(filepath.Dir(s.path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s *Store) Initialize(name, password string) error {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 128 || len(password) < 12 || len(password) > 1024 {
		return errors.New("username must contain 1–128 bytes; password 12–1024 bytes")
	}
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	hash, err := pbkdf2.Key(sha256.New, password, salt, passwordIterations, 32)
	if err != nil {
		return err
	}
	return s.update(func(d *Data) error {
		if d.SchemaVersion != 0 {
			return errors.New("already initialized")
		}
		*d = Data{SchemaVersion: 1, ServerID: randomID(), User: User{ID: randomID(), Name: name, Salt: salt, PasswordHash: hash, Iterations: passwordIterations, Configuration: map[string]json.RawMessage{}, Settings: map[string]json.RawMessage{}}, Sessions: map[string]Session{}}
		return nil
	})
}

func tokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func (s *Store) Login(name, password string, client Session) (string, Session, error) {
	s.mu.RLock()
	u := s.data.User
	s.mu.RUnlock()
	if u.ID == "" {
		return "", Session{}, ErrCredentials
	}
	hash, err := pbkdf2.Key(sha256.New, password, u.Salt, u.Iterations, 32)
	if err != nil {
		return "", Session{}, err
	}
	valid := subtle.ConstantTimeCompare(hash, u.PasswordHash)
	if !strings.EqualFold(name, u.Name) || valid != 1 {
		return "", Session{}, ErrCredentials
	}
	token := randomID() + randomID()
	client.ID, client.UserID = randomID(), u.ID
	client.ExpiresAt = time.Now().UTC().Add(30 * 24 * time.Hour)
	err = s.update(func(d *Data) error {
		for key, session := range d.Sessions {
			if !session.ExpiresAt.After(time.Now()) {
				delete(d.Sessions, key)
			}
		}
		if len(d.Sessions) >= 64 {
			return ErrSessionLimit
		}
		d.Sessions[tokenHash(token)] = client
		return nil
	})
	return token, client, err
}

func (s *Store) Authenticate(token string) (Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if token == "" {
		return Session{}, ErrSession
	}
	v, ok := s.data.Sessions[tokenHash(token)]
	if !ok || !v.ExpiresAt.After(time.Now()) {
		return Session{}, ErrSession
	}
	return v, nil
}

func (s *Store) Logout(token string) error {
	return s.update(func(d *Data) error { delete(d.Sessions, tokenHash(token)); return nil })
}

// Change checks the session again under the write lock to prevent a concurrent
// logout from being followed by a successful settings mutation.
func (s *Store) Change(token string, fn func(*Data, *Session)) error {
	return s.update(func(d *Data) error {
		key := tokenHash(token)
		session, ok := d.Sessions[key]
		if !ok || !session.ExpiresAt.After(time.Now()) {
			return ErrSession
		}
		fn(d, &session)
		d.Sessions[key] = session
		return nil
	})
}

// SetItem updates one item's playback state under the write lock, re-checking
// the session so a concurrent logout cannot be followed by a successful write.
func (s *Store) SetItem(token, itemID string, fn func(*ItemState)) error {
	return s.SetItemSession(token, itemID, func(item *ItemState, _ *Session) { fn(item) })
}

// SetItemSession saves playback lifecycle and item state in one transaction.
func (s *Store) SetItemSession(token, itemID string, fn func(*ItemState, *Session)) error {
	if itemID == "" {
		return errors.New("item id is required")
	}
	return s.update(func(d *Data) error {
		key := tokenHash(token)
		session, ok := d.Sessions[key]
		if !ok || !session.ExpiresAt.After(time.Now()) {
			return ErrSession
		}
		if d.User.Items == nil {
			d.User.Items = map[string]ItemState{}
		}
		current, exists := d.User.Items[itemID]
		if !exists && len(d.User.Items) >= maxTrackedItems {
			return ErrStateLimit
		}
		fn(&current, &session)
		d.Sessions[key] = session
		if current == (ItemState{}) {
			delete(d.User.Items, itemID)
			return nil
		}
		d.User.Items[itemID] = current
		return nil
	})
}

func ReadPassword(r io.Reader) (string, error) {
	b, err := io.ReadAll(io.LimitReader(r, 1027))
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r"), nil
}
