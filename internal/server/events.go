package server

import (
	"encoding/json"
	"slices"

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
	if s.hls != nil {
		s.hls.StopOwner(id)
	}
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
	s.publish(object{"MessageType": "UserDataChanged", "Data": object{
		"UserId": userID, "UserDataList": []object{data},
	}}, func(sock *socket) bool { return sock.userID == userID })
}

// publishLibraryChange tells every connection how a rescan changed the
// catalog, so that Emby Web refreshes the screens showing it.
func (s *Server) publishLibraryChange(old, next *media.Catalog) {
	if data := libraryChange(old, next); data != nil {
		s.publish(object{"MessageType": "LibraryChanged", "Data": data}, func(*socket) bool { return true })
	}
}

// libraryChange lists what changed between two catalogs in the fields Emby
// Web's LibraryChanged handler reads, or returns nil when nothing did. A list
// whose parent is in FoldersAddedTo, FoldersRemovedFrom or CollectionFolders
// reloads, as does one showing a removed item. The field names come from the
// client, not from a captured Emby message.
func libraryChange(old, next *media.Catalog) object {
	added, removed, updated := set{}, set{}, set{}
	addedTo, removedFrom, libraries := set{}, set{}, set{}
	previous, current := catalogEntries(old), catalogEntries(next)
	for id, e := range current {
		was, ok := previous[id]
		switch {
		case !ok:
			added[id], addedTo[e.parent], libraries[e.library] = true, true, true
		case was.item.Size != e.item.Size || !was.item.Modified.Equal(e.item.Modified) || was.item.RunTimeTicks != e.item.RunTimeTicks:
			updated[id], libraries[e.library] = true, true
		}
	}
	for id, e := range previous {
		if _, ok := current[id]; !ok {
			removed[id], removedFrom[e.parent], libraries[e.library] = true, true, true
		}
	}
	if len(added)+len(removed)+len(updated) == 0 {
		return nil
	}
	return object{"ItemsAdded": added.sorted(), "ItemsRemoved": removed.sorted(), "ItemsUpdated": updated.sorted(),
		"FoldersAddedTo": addedTo.sorted(), "FoldersRemovedFrom": removedFrom.sorted(), "CollectionFolders": libraries.sorted()}
}

type set map[string]bool

func (s set) sorted() []string {
	ids := make([]string, 0, len(s))
	for id := range s {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

type catalogEntry struct {
	item            media.Item
	parent, library string
}

// catalogEntries indexes a catalog's items and folders with their parent and
// library: movies sit directly in the movie library, series in the TV
// library.
func catalogEntries(c *media.Catalog) map[string]catalogEntry {
	entries := map[string]catalogEntry{}
	if c == nil {
		return entries
	}
	for _, items := range [][]media.Item{c.Items, c.Folders} {
		for _, item := range items {
			e := catalogEntry{item: item, parent: item.ParentID, library: c.ID}
			if item.Kind == "Series" || item.Kind == "Season" || item.Kind == "Episode" {
				e.library = c.SeriesLibraryID()
			}
			if e.parent == "" {
				e.parent = e.library
			}
			entries[item.ID] = e
		}
	}
	return entries
}

// publish queues message on the connections match selects.
func (s *Server) publish(message object, match func(*socket) bool) {
	body, err := json.Marshal(message)
	if err != nil {
		return
	}
	s.socketMu.Lock()
	defer s.socketMu.Unlock()
	for sock := range s.connections {
		if match(sock) {
			s.enqueue(sock, body)
		}
	}
}

// publishUserConfiguration sends the user's connections the user as each of
// them sees it after its settings changed. Emby Web replaces its cached user
// with the message's, so the subtitle and audio preferences saved on one
// device apply to playback started on another without a reload.
func (s *Server) publishUserConfiguration(userID string) {
	d := s.store.Snapshot()
	s.socketMu.Lock()
	defer s.socketMu.Unlock()
	for sock := range s.connections {
		if sock.userID != userID {
			continue
		}
		// The avatar tag in the user is signed for the connection's token.
		body, err := json.Marshal(object{"MessageType": "UserConfigurationUpdated", "Data": s.userDTO(d, sock.token)})
		if err == nil {
			s.enqueue(sock, body)
		}
	}
}

// enqueue queues body on sock. Caller holds s.socketMu.
func (s *Server) enqueue(sock *socket, body []byte) {
	select {
	case sock.out <- body:
	default:
		// Disconnect on overflow instead of silently losing events or blocking
		// API writes. A reconnecting client must reload its current state.
		_ = sock.conn.Close()
		delete(s.connections, sock)
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
