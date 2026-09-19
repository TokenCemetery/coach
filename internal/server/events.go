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
	// Keep commit, snapshot and enqueue ordered across concurrent API requests.
	s.itemMu.Lock()
	defer s.itemMu.Unlock()
	if err := s.store.SetItem(token, item.ID, change); err != nil {
		return nil, err
	}
	snapshot := s.store.Snapshot()
	data := itemUserData(snapshot.User.Items[item.ID], item.RunTimeTicks)
	data["ItemId"] = item.ID
	s.publishUserData(snapshot.User.ID, data)
	return data, nil
}
