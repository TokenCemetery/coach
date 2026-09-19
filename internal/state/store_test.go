package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Initialize("viewer", "test-only-password"); err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func TestStateProtectionAndExpiry(t *testing.T) {
	s, dir := testStore(t)
	if second, err := Open(dir); err == nil {
		second.Close()
		t.Fatal("second process could open state")
	}
	if s.Initialize("replacement", "another-test-password") == nil {
		t.Fatal("initialized twice")
	}
	token, _, err := s.Login("viewer", "test-only-password", Session{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte(token)) || bytes.Contains(b, []byte("test-only-password")) {
		t.Fatal("plaintext secret persisted")
	}
	info, err := os.Stat(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatal("unsafe state permissions")
	}
	if err := s.Change(token, func(d *Data, session *Session) { session.ExpiresAt = time.Now().Add(-time.Second) }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(token); !errors.Is(err, ErrSession) {
		t.Fatal("expired session accepted")
	}
	if err := s.Change(token, func(d *Data, session *Session) { t.Fatal("expired session mutated state") }); !errors.Is(err, ErrSession) {
		t.Fatal(err)
	}
}

func TestFailedWriteDoesNotPublishState(t *testing.T) {
	s, dir := testStore(t)
	token, _, err := s.Login("viewer", "test-only-password", Session{})
	if err != nil {
		t.Fatal(err)
	}
	original := s.path
	s.path = filepath.Join(dir, "missing-parent", "state.json")
	err = s.Change(token, func(d *Data, session *Session) { d.User.Settings["theme"] = json.RawMessage(`"changed"`) })
	if err == nil {
		t.Fatal("write should fail")
	}
	if len(s.Snapshot().User.Settings) != 0 {
		t.Fatal("failed write changed memory")
	}
	s.path = original
}

func TestCorruptAndFutureStateAreNotReset(t *testing.T) {
	for _, content := range []string{"{invalid", `{"SchemaVersion":99}`, `{"SchemaVersion":1}`} {
		dir := t.TempDir()
		file := filepath.Join(dir, "state.json")
		if err := os.WriteFile(file, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if s, err := Open(dir); err == nil {
			s.Close()
			t.Fatal("invalid state accepted")
		}
		b, err := os.ReadFile(file)
		if err != nil || string(b) != content {
			t.Fatal("invalid state overwritten")
		}
	}
}

func TestConcurrentSettingsAndLogout(t *testing.T) {
	s, _ := testStore(t)
	token, _, err := s.Login("viewer", "test-only-password", Session{})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.Change(token, func(d *Data, session *Session) { d.User.Settings["theme"] = json.RawMessage(`"dark"`) })
			_, _ = s.Authenticate(token)
			_ = s.Snapshot()
		}()
	}
	if err := s.Logout(token); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if _, err := s.Authenticate(token); err == nil {
		t.Fatal("session resurrected")
	}
}

func TestStateSizeLimitDoesNotChangeCommittedData(t *testing.T) {
	s, dir := testStore(t)
	token, _, err := s.Login("viewer", "test-only-password", Session{})
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	err = s.Change(token, func(d *Data, session *Session) {
		d.User.Settings["oversized"] = json.RawMessage(`"` + strings.Repeat("a", maxStateBytes) + `"`)
	})
	if !errors.Is(err, ErrStateLimit) {
		t.Fatal("expected state size limit")
	}
	after, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || len(s.Snapshot().User.Settings) != 0 {
		t.Fatal("rejected update changed state")
	}
}
