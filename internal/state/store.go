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

// Errors returned by Store.
var (
	ErrCredentials  = errors.New("invalid credentials")
	ErrSession      = errors.New("invalid or expired session")
	ErrSessionLimit = errors.New("session limit reached")
	ErrStateLimit   = errors.New("state size limit reached")
	ErrPassword     = errors.New("password must contain 12–1024 bytes")
)

// User is the single local account with its settings and playback state.
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
	// Image is the uploaded avatar, absent until one is uploaded.
	Image *UserImage `json:",omitempty"`
}

// ItemState is the durable part of an item's UserData.
type ItemState struct {
	PositionTicks int64
	PlayCount     int
	Played        bool
	IsFavorite    bool
	LastPlayed    time.Time
	// LastSearched records when the user opened the item from search results.
	LastSearched time.Time
	// HiddenFromResume keeps a next-up episode out of Continue Watching after
	// "Remove from Continue Watching", until the item is played again.
	HiddenFromResume bool `json:",omitempty"`
}

// maxTrackedItems bounds the state file independently of its byte limit.
const maxTrackedItems = 20000

// Session is an authenticated client. Stored keyed by token hash, never by token.
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

// Data is the persisted state file contents (schema v1).
type Data struct {
	SchemaVersion int
	ServerID      string
	User          User
	Sessions      map[string]Session
}

// Store holds the state in memory and writes every change to disk atomically.
type Store struct {
	mu   sync.RWMutex
	path string
	lock *os.File
	data Data
	// imageMu serializes avatar changes, so a file is removed only once the
	// committed state no longer references it.
	imageMu sync.Mutex
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
	lock, err := os.OpenFile(filepath.Join(dir, "state.lock"), os.O_CREATE|os.O_RDWR, 0600) //nolint:gosec // dir is the operator-selected data directory|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("data directory is already in use: %w", err)
	}
	removeInterruptedWrites(dir)
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
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// removeInterruptedWrites deletes the temp files of writes cut short by a
// crash, which skip their deferred removal. The caller holds the directory
// lock, so no write is in progress.
func removeInterruptedWrites(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.Type().IsRegular() && (strings.HasPrefix(name, ".state-") || strings.HasPrefix(name, ".user-image-")) {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}

// Close releases the data directory lock.
func (s *Store) Close() error { return s.lock.Close() }

// Initialized reports whether a user has been created.
func (s *Store) Initialized() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.SchemaVersion != 0
}

// Snapshot returns a deep copy of the current state.
func (s *Store) Snapshot() Data {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return clone(s.data)
}

func clone(d Data) Data {
	b, _ := json.Marshal(d)
	var out Data
	_ = json.Unmarshal(b, &out)
	return out
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
	// After a successful rename this removal fails harmlessly.
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err = f.Write(b); err != nil {
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
	if err = os.Rename(f.Name(), s.path); err != nil {
		return err
	}
	// Rename already committed: keep memory consistent even if directory fsync fails.
	s.data = next
	dir, err := os.Open(filepath.Dir(s.path))
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

// Initialize creates the single local user. It fails if one already exists.
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

// Login verifies the credentials and stores a new 30-day session. It returns the
// session token, which is not persisted.
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

// ChangePassword replaces the password after verifying the current one. The
// calling session stays signed in; every other session is revoked, and their
// IDs are returned so open connections can be closed.
func (s *Store) ChangePassword(token, current, next string) ([]string, error) {
	if len(next) < 12 || len(next) > 1024 {
		return nil, ErrPassword
	}
	s.mu.RLock()
	u := s.data.User
	s.mu.RUnlock()
	hash, err := pbkdf2.Key(sha256.New, current, u.Salt, u.Iterations, 32)
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare(hash, u.PasswordHash) != 1 {
		return nil, ErrCredentials
	}
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	nextHash, err := pbkdf2.Key(sha256.New, next, salt, passwordIterations, 32)
	if err != nil {
		return nil, err
	}
	revoked := []string{}
	err = s.update(func(d *Data) error {
		key := tokenHash(token)
		if session, ok := d.Sessions[key]; !ok || !session.ExpiresAt.After(time.Now()) {
			return ErrSession
		}
		// A concurrent change must not be overwritten with a stale verification.
		if subtle.ConstantTimeCompare(d.User.PasswordHash, u.PasswordHash) != 1 {
			return ErrCredentials
		}
		d.User.Salt, d.User.PasswordHash, d.User.Iterations = salt, nextHash, passwordIterations
		for k, session := range d.Sessions {
			if k != key {
				revoked = append(revoked, session.ID)
				delete(d.Sessions, k)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return revoked, nil
}

// Authenticate returns the unexpired session for token.
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

// Logout revokes the session for token.
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

// SetItems applies fn to several items in one transaction: either every item
// is saved, or none is when the session is invalid or the tracked-item limit
// would be exceeded. Items fn leaves empty are dropped and never count toward
// the limit. It returns the resulting state of each item.
func (s *Store) SetItems(token string, itemIDs []string, fn func(*ItemState)) (map[string]ItemState, error) {
	saved := map[string]ItemState{}
	err := s.update(func(d *Data) error {
		session, ok := d.Sessions[tokenHash(token)]
		if !ok || !session.ExpiresAt.After(time.Now()) {
			return ErrSession
		}
		if d.User.Items == nil {
			d.User.Items = map[string]ItemState{}
		}
		added := 0
		for _, id := range itemIDs {
			current, exists := d.User.Items[id]
			fn(&current)
			saved[id] = current
			if !exists && current != (ItemState{}) {
				added++
			}
		}
		if added > 0 && len(d.User.Items)+added > maxTrackedItems {
			return ErrStateLimit
		}
		for id, current := range saved {
			if current == (ItemState{}) {
				delete(d.User.Items, id)
			} else {
				d.User.Items[id] = current
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return saved, nil
}

// ReadPassword reads a bounded password and strips one trailing newline.
// Initialize enforces the length limits.
func ReadPassword(r io.Reader) (string, error) {
	b, err := io.ReadAll(io.LimitReader(r, 1027))
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r"), nil
}
