package state

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

func imageMAC(key, sessionID, itemID, revision string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte("coach-image-v1\x00" + sessionID + "\x00" + itemID + "\x00" + revision))
	return hex.EncodeToString(mac.Sum(nil))
}

// ImageTag grants access only to this item's image revision. It is not an API
// token. Using the stored token hash as the MAC key needs no extra secret/schema.
func (s *Store) ImageTag(token, itemID, revision string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := tokenHash(token)
	session, ok := s.data.Sessions[key]
	if token == "" || !ok || !session.ExpiresAt.After(time.Now()) {
		return ""
	}
	return revision + "." + session.ID + "." + imageMAC(key, session.ID, itemID, revision)
}

// AuthenticateImageTag rechecks session existence and expiry on every request,
// including cache validation. Session lookup is bounded by the 64-session limit.
func (s *Store) AuthenticateImageTag(itemID, tag string) (Session, string, error) {
	parts := strings.Split(tag, ".")
	if len(parts) != 3 || len(parts[0]) != 32 || len(parts[1]) != 32 || len(parts[2]) != 64 {
		return Session{}, "", ErrSession
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for key, session := range s.data.Sessions {
		if session.ID != parts[1] || !session.ExpiresAt.After(time.Now()) {
			continue
		}
		if hmac.Equal([]byte(parts[2]), []byte(imageMAC(key, session.ID, itemID, parts[0]))) {
			return session, parts[0], nil
		}
	}
	return Session{}, "", ErrSession
}
