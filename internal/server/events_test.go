package server

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/TokenCemetery/coach/internal/media"
	"github.com/TokenCemetery/coach/internal/state"
)

func openEventSocket(t *testing.T, server *httptest.Server, token string) (net.Conn, *bufio.Reader) {
	t.Helper()
	status, _, conn, reader := socketRequest(t, server, "?api_key="+token, "13", exampleKey)
	if status != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %d", status)
	}
	if kind, _ := expectSocketMessage(t, reader); kind != "ForceKeepAlive" {
		t.Fatal("initial message must precede events")
	}
	return conn, reader
}

func expectUserData(t *testing.T, reader *bufio.Reader, user string) map[string]any {
	t.Helper()
	kind, raw := expectSocketMessage(t, reader)
	var data struct {
		UserId       string
		UserDataList []map[string]any
	}
	if kind != "UserDataChanged" || json.Unmarshal(raw, &data) != nil || data.UserId != user || len(data.UserDataList) != 1 || data.UserDataList[0]["ItemId"] != "movie" {
		t.Fatal("invalid UserDataChanged envelope")
	}
	return data.UserDataList[0]
}

func expectDisconnected(t *testing.T, conn net.Conn, reader *bufio.Reader) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	_, err := reader.ReadByte()
	if err == nil {
		t.Fatal("received data after connection should have closed")
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		t.Fatal("connection remained open")
	}
}

func TestUserDataEventsAndSessionLifecycle(t *testing.T) {
	store, _, _ := newTestServer(t)
	api := New(store, "test", nil, &media.Catalog{Items: []media.Item{playbackMovie()}})
	t.Cleanup(api.Close)
	h := api.Handler()
	server := httptest.NewServer(h)
	defer server.Close()
	token1, token2 := login(t, h), login(t, h)
	user := store.Snapshot().User.ID
	c1, r1 := openEventSocket(t, server, token1)
	c2, r2 := openEventSocket(t, server, token2)
	for _, tc := range []struct {
		method, path, body, field string
		value                     any
		status                    int
	}{
		{"POST", "/Users/" + user + "/FavoriteItems/movie", "", "IsFavorite", true, 200},
		{"DELETE", "/Users/" + user + "/FavoriteItems/movie", "", "IsFavorite", false, 200},
		{"POST", "/Users/" + user + "/FavoriteItems/movie", "", "IsFavorite", true, 200},
		{"POST", "/Users/" + user + "/FavoriteItems/movie/Delete", "", "IsFavorite", false, 200},
		{"POST", "/Users/" + user + "/PlayedItems/movie", "", "Played", true, 200},
		{"DELETE", "/Users/" + user + "/PlayedItems/movie", "", "Played", false, 200},
		{"POST", "/Users/" + user + "/PlayedItems/movie", "", "Played", true, 200},
		{"POST", "/Users/" + user + "/PlayedItems/movie/Delete", "", "Played", false, 200},
		{"POST", "/Sessions/Playing", `{"ItemId":"movie","PositionTicks":1000000}`, "PlaybackPositionTicks", float64(1000000), 204},
		{"POST", "/Sessions/Playing/Progress", `{"ItemId":"movie","PositionTicks":2500000}`, "PlaybackPositionTicks", float64(2500000), 204},
		{"POST", "/Sessions/Playing/Stopped", `{"ItemId":"movie","PositionTicks":10000000}`, "Played", true, 204},
	} {
		w := request(h, tc.method, tc.path, "application/json", tc.body, token2)
		expectStatus(t, w, tc.status)
		for _, r := range []*bufio.Reader{r1, r2} {
			data := expectUserData(t, r, user)
			if data[tc.field] != tc.value {
				t.Fatalf("incorrect %s in event", tc.field)
			}
		}
	}
	// An unsuccessful mutation must produce no event (the next frame is pong).
	expectStatus(t, request(h, "POST", "/Users/other/PlayedItems/movie", "", "", token2), 403)
	writeClientFrame(t, c1, opPing, []byte("barrier"))
	if opcode, _ := readServerFrame(t, r1); opcode != opPong {
		t.Fatal("failed request emitted an event")
	}
	c1b, r1b := openEventSocket(t, server, token1)
	expectStatus(t, request(h, "POST", "/Sessions/Logout", "", "", token1), 204)
	expectDisconnected(t, c1, r1)
	expectDisconnected(t, c1b, r1b)
	writeClientFrame(t, c2, opPing, []byte("alive"))
	if opcode, _ := readServerFrame(t, r2); opcode != opPong {
		t.Fatal("logout affected another session")
	}
	// Expiry must be checked before delivering even an already queued event.
	if err := store.Change(token2, func(_ *state.Data, session *state.Session) {
		session.ExpiresAt = time.Now().Add(-time.Second)
	}); err != nil {
		t.Fatal(err)
	}
	api.publishUserData(user, object{"ItemId": "movie"})
	expectDisconnected(t, c2, r2)
	c3, r3 := openEventSocket(t, server, login(t, h))
	api.Close()
	api.Close()
	// Shutdown says goodbye first (TestServerShuttingDown), then disconnects.
	if kind, _ := expectSocketMessage(t, r3); kind != "ServerShuttingDown" {
		t.Fatalf("message %q, want ServerShuttingDown", kind)
	}
	expectCloseFrame(t, r3, 1001)
	expectDisconnected(t, c3, r3)
	deadline := time.Now().Add(time.Second)
	for api.sockets.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if api.sockets.Load() != 0 {
		t.Fatal("socket handlers did not terminate after shutdown")
	}
}

func TestEventOrderAndFailedPersistence(t *testing.T) {
	store, _, dir := newTestServer(t)
	api := New(store, "test", nil, nil)
	t.Cleanup(api.Close)
	token := login(t, api.Handler())
	session, _ := store.Authenticate(token)
	server, client := net.Pipe()
	defer func() { _ = client.Close() }()
	sock := &socket{conn: server, out: make(chan []byte, socketQueue), token: token, userID: session.UserID, sessionID: session.ID}
	if !api.registerSocket(sock) {
		t.Fatal("registration failed")
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if _, err := api.updateItem(token, playbackMovie(), func(st *state.ItemState) { st.PlayCount++ }); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	for i := 1; i <= 16; i++ {
		var message struct {
			Data struct{ UserDataList []struct{ PlayCount int } }
		}
		if json.Unmarshal(<-sock.out, &message) != nil || len(message.Data.UserDataList) != 1 || message.Data.UserDataList[0].PlayCount != i {
			t.Fatal("events are not in commit order")
		}
	}
	// Replace only this test's state path with a directory to fail atomic rename.
	statePath := filepath.Join(dir, "state.json")
	backup := filepath.Join(dir, "state.backup")
	if err := os.Rename(statePath, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(statePath, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := api.updateItem(token, playbackMovie(), func(st *state.ItemState) { st.PlayCount++ }); err == nil {
		t.Fatal("persistence failure was ignored")
	}
	if len(sock.out) != 0 || store.Snapshot().User.Items["movie"].PlayCount != 16 {
		t.Fatal("failed commit published state or event")
	}
}

func TestEventQueueBoundAndUserIsolation(t *testing.T) {
	store, _, _ := newTestServer(t)
	api := New(store, "test", nil, nil)
	t.Cleanup(api.Close)
	token := login(t, api.Handler())
	session, _ := store.Authenticate(token)
	makeSocket := func(user string) (*socket, net.Conn) {
		a, b := net.Pipe()
		t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
		return &socket{conn: a, out: make(chan []byte, socketQueue), token: token, userID: user, sessionID: session.ID}, b
	}
	slow, slowPeer := makeSocket(session.UserID)
	fast, _ := makeSocket(session.UserID)
	other, _ := makeSocket("other-user")
	// Insert a synthetic foreign user to verify routing; registration itself
	// would refuse this mismatched identity in today's single-user server.
	for _, sock := range []*socket{slow, fast, other} {
		api.connections[sock] = struct{}{}
	}
	for range socketQueue + 1 {
		api.publishUserData(session.UserID, object{"ItemId": "movie"})
		select {
		case <-fast.out:
		default:
			t.Fatal("slow subscriber blocked another subscriber")
		}
	}
	if len(slow.out) != socketQueue || len(other.out) != 0 {
		t.Fatal("unbounded queue or event sent to another user")
	}
	if _, exists := api.connections[slow]; exists {
		t.Fatal("overflowed subscriber was retained")
	}
	_ = slowPeer.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := slowPeer.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("slow subscriber: %v", err)
	}
	if api.registerSocket(other) {
		t.Fatal("foreign identity accepted")
	}
	if err := store.Logout(token); err != nil {
		t.Fatal(err)
	}
	if api.registerSocket(fast) {
		t.Fatal("revoked token accepted after auth race")
	}
}

func TestLibraryChangedAfterRescan(t *testing.T) {
	store, _, _ := newTestServer(t)
	movie := playbackMovie()
	series := media.Item{ID: "series", Kind: "Series", Name: "Show"}
	series.ParentID = (&media.Catalog{ID: "movies"}).SeriesLibraryID()
	season := media.Item{ID: "season", Kind: "Season", ParentID: "series"}
	episode := media.Item{ID: "episode", Kind: "Episode", ParentID: "season", Size: 1}
	api := New(store, "test", nil, &media.Catalog{ID: "movies", Items: []media.Item{movie, episode}, Folders: []media.Item{series, season}})
	t.Cleanup(api.Close)
	h := api.Handler()
	server := httptest.NewServer(h)
	defer server.Close()
	_, reader := openEventSocket(t, server, login(t, h))

	changed := movie
	changed.Size++
	added := media.Item{ID: "added", Kind: "Movie", Size: 1}
	next := &media.Catalog{ID: "movies", Items: []media.Item{changed, added}, Folders: []media.Item{series, season}}
	api.SetCatalog(next)
	kind, raw := expectSocketMessage(t, reader)
	var data map[string][]string
	if kind != "LibraryChanged" || json.Unmarshal(raw, &data) != nil {
		t.Fatalf("message %s %s", kind, raw)
	}
	tvLibrary := next.SeriesLibraryID()
	for field, want := range map[string][]string{
		"ItemsAdded": {"added"}, "ItemsRemoved": {"episode"}, "ItemsUpdated": {"movie"},
		"FoldersAddedTo": {"movies"}, "FoldersRemovedFrom": {"season"}, "CollectionFolders": sortedIDs("movies", tvLibrary),
	} {
		if got := data[field]; !slices.Equal(got, want) {
			t.Fatalf("%s = %v, want %v", field, got, want)
		}
	}
	// An unchanged catalog sends nothing; the next event is user data.
	api.SetCatalog(&media.Catalog{ID: "movies", Items: []media.Item{changed, added}, Folders: []media.Item{series, season}})
	expectStatus(t, request(h, "POST", "/Users/"+store.Snapshot().User.ID+"/FavoriteItems/movie", "", "", login(t, h)), 200)
	expectUserData(t, reader, store.Snapshot().User.ID)
}

func sortedIDs(ids ...string) []string {
	slices.Sort(ids)
	return ids
}

func TestUserConfigurationUpdated(t *testing.T) {
	store, _, _ := newTestServer(t)
	api := New(store, "test", nil, &media.Catalog{Items: []media.Item{playbackMovie()}})
	t.Cleanup(api.Close)
	h := api.Handler()
	server := httptest.NewServer(h)
	defer server.Close()
	token1, token2 := login(t, h), login(t, h)
	user := store.Snapshot().User.ID
	_, reader := openEventSocket(t, server, token2)
	// UserSettings are not part of the user and send nothing.
	expectStatus(t, request(h, "POST", "/UserSettings/"+user+"/Partial", "application/json", `{"theme":"dark"}`, token1), 204)
	expectStatus(t, request(h, "POST", "/Users/"+user+"/Configuration/Partial", "application/json", `{"SubtitleMode":"Always"}`, token1), 204)
	kind, raw := expectSocketMessage(t, reader)
	var data struct {
		Id            string
		Configuration map[string]any
		Policy        map[string]any
	}
	if kind != "UserConfigurationUpdated" || json.Unmarshal(raw, &data) != nil || data.Id != user ||
		data.Configuration["SubtitleMode"] != "Always" || data.Configuration["PlayDefaultAudioTrack"] != true || data.Policy == nil {
		t.Fatalf("message %s %s", kind, raw)
	}
}

// A stopping server tells each client before it closes the connection, and
// Emby Web shows the notice (#25).
func TestServerShuttingDown(t *testing.T) {
	store, _, _ := newTestServer(t)
	api := New(store, "test", nil, &media.Catalog{Items: []media.Item{playbackMovie()}})
	h := api.Handler()
	server := httptest.NewServer(h)
	defer server.Close()
	_, r1 := openEventSocket(t, server, login(t, h))
	_, r2 := openEventSocket(t, server, login(t, h))
	api.Close()
	for _, reader := range []*bufio.Reader{r1, r2} {
		if kind, _ := expectSocketMessage(t, reader); kind != "ServerShuttingDown" {
			t.Fatalf("message %q, want ServerShuttingDown", kind)
		}
		expectCloseFrame(t, reader, 1001)
	}
}
