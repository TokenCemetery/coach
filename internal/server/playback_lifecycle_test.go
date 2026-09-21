package server

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/TokenCemetery/coach/internal/media"
	"github.com/TokenCemetery/coach/internal/state"
)

func TestPlaybackReportRetriesSurviveRestart(t *testing.T) {
	store, _, dir := newTestServer(t)
	catalog := &media.Catalog{ID: "movies", Items: []media.Item{{ID: "movie", RunTimeTicks: 100000000}}}
	h := New(store, "test", nil, catalog).Handler()
	token := login(t, h)
	report := func(endpoint, playID string, position int64) {
		t.Helper()
		body, err := json.Marshal(object{"ItemId": "movie", "PlaySessionId": playID, "PositionTicks": position})
		if err != nil {
			t.Fatal(err)
		}
		expectStatus(t, request(h, "POST", "/Sessions/Playing"+endpoint, "application/json", string(body), token), 204)
	}
	report("", "first", 0)
	report("/Progress", "first", 25000000)
	report("", "first", 0)
	before := store.Snapshot().User.Items["movie"]
	if before.PositionTicks != 25000000 || before.PlayCount != 1 {
		t.Fatal("duplicate start changed progress/count")
	}
	report("/Stopped", "first", 100000000)
	finished := store.Snapshot().User.Items["movie"]
	if !finished.Played || finished.PositionTicks != 0 {
		t.Fatal("completion not saved")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	h = New(store, "test", nil, catalog).Handler()
	report("/Progress", "first", 25000000)
	report("", "first", 0)
	report("/Stopped", "first", 25000000)
	if got := store.Snapshot().User.Items["movie"]; got != finished {
		t.Fatal("old report reopened completed playback")
	}
	report("", "second", 0)
	report("/Progress", "second", 40000000)
	current := store.Snapshot().User.Items["movie"]
	report("", "first", 0)
	report("/Stopped", "first", 100000000)
	if got := store.Snapshot().User.Items["movie"]; got != current || got.PlayCount != 2 {
		t.Fatal("old playback changed the current play")
	}
	report("/Progress", "second", 10000000)
	if store.Snapshot().User.Items["movie"].PositionTicks != 10000000 {
		t.Fatal("backward seek refused")
	}
	// Reports without IDs keep compatibility with older clients.
	report("/Progress", "", 20000000)
	if store.Snapshot().User.Items["movie"].PositionTicks != 20000000 {
		t.Fatal("legacy report refused")
	}
}

func TestPlaybackRetryWindowBounded(t *testing.T) {
	var session state.Session
	for i := 0; i < 100; i++ {
		if !acceptPlayReport(&session, "movie", strconv.Itoa(i), "start") {
			t.Fatal("new start refused")
		}
	}
	if len(session.RecentPlays) != 64 || session.RecentPlays[0].ID != "36" {
		t.Fatal("retry window not bounded")
	}
	if acceptPlayReport(&session, "other", "99", "progress") || acceptPlayReport(&session, "movie", "98", "progress") || acceptPlayReport(&session, "movie", "missing", "stop") {
		t.Fatal("foreign/stale report accepted")
	}
}
