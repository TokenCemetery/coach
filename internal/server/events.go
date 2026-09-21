package server

import (
	"encoding/json"

	"github.com/TokenCemetery/coach/internal/media"
	"github.com/TokenCemetery/coach/internal/state"
)

func (s *Server) registerSocket(sock *socket) bool {
	s.socketMu.Lock()
	defer s.socketMu.Unlock()
	// Recheck after upgrade: logout/shutdown may race with the HTTP auth check.
	session, err := s.store.Authenticate(sock.token)
	if s.closing || err != nil || session.ID != sock.sessionID || session.UserID != sock.userID {
		return false
	}
	s.connections[sock] = struct{}{}
	return true
}

func (s *Server) removeSocket(sock *socket) {
	s.socketMu.Lock()
	defer s.socketMu.Unlock()
	delete(s.connections, sock)
}

func (s *Server) disconnectSession(id string) {
	s.socketMu.Lock()
	defer s.socketMu.Unlock()
	for sock := range s.connections {
		if sock.sessionID == id {
			_ = sock.conn.Close()
			delete(s.connections, sock)
		}
	}
}

// Close stops hijacked connections, which http.Server.Shutdown does not own.
// The caller still owns the HTTP server, store, media root and web assets.
func (s *Server) Close() {
	s.socketMu.Lock()
	defer s.socketMu.Unlock()
	s.closing = true
	for sock := range s.connections {
		_ = sock.conn.Close()
		delete(s.connections, sock)
	}
}

func (s *Server) publishUserData(userID string, data object) {
	body, err := json.Marshal(object{"MessageType": "UserDataChanged", "Data": object{
		"UserId": userID, "UserDataList": []object{data},
	}})
	if err != nil {
		return
	}
	s.socketMu.Lock()
	defer s.socketMu.Unlock()
	for sock := range s.connections {
		if sock.userID != userID {
			continue
		}
		select {
		case sock.out <- body:
		default:
			// Disconnect on overflow instead of silently losing events or blocking
			// API writes. A reconnecting client must reload its current state.
			_ = sock.conn.Close()
			delete(s.connections, sock)
		}
	}
}

func (s *Server) updateItem(token string, item media.Item, change func(*state.ItemState)) (object, error) {
	return s.updateItemSession(token, item, func(st *state.ItemState, _ *state.Session) { change(st) })
}

func (s *Server) updateItemSession(token string, item media.Item, change func(*state.ItemState, *state.Session)) (object, error) {
	// Keep commit and enqueue ordered across concurrent API requests.
	s.itemMu.Lock()
	defer s.itemMu.Unlock()
	session, err := s.store.Authenticate(token)
	if err != nil {
		return nil, err
	}
	var saved state.ItemState
	applied := false
	if err := s.store.SetItemSession(token, item.ID, func(st *state.ItemState, session *state.Session) {
		change(st, session)
		saved, applied = *st, true
	}); err != nil {
		return nil, err
	}
	if !applied {
		return nil, state.ErrStateLimit
	}
	// Capture only this item, not a JSON clone of the entire store per event.
	data := itemUserData(saved, item.RunTimeTicks)
	data["ItemId"] = item.ID
	s.publishUserData(session.UserID, data)
	return data, nil
}
