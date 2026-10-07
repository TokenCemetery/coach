package server

import (
	"encoding/json"
	"strconv"
	"strings"
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
	defer func() { _ = store.Close() }()
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
	for i := range 100 {
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

// TestPlayReportProblems names the rule that drops a report, which the server
// logs while still answering 204 (issue 49).
func TestPlayReportProblems(t *testing.T) {
	var session state.Session
	for _, tc := range []struct{ item, play, event, problem string }{
		{"movie", "", "progress", ""},
		{"movie", "a", "start", ""},
		{"movie", "a", "start", "repeated start"},
		{"movie", "missing", "progress", "unknown play session"},
		{"other", "a", "progress", "play session belongs to another item"},
		{"movie", "b", "start", ""},
		{"movie", "a", "stop", "not the newest play session"},
		{"movie", "b", "stop", ""},
		{"movie", "b", "progress", "play session already stopped"},
	} {
		if got := playReportProblem(&session, tc.item, tc.play, tc.event); got != tc.problem {
			t.Fatalf("%s %s %s: got %q, want %q", tc.item, tc.play, tc.event, got, tc.problem)
		}
	}
}

// Switching quality or audio track asks PlaybackInfo again with the current
// play in CurrentPlaySessionId. Emby Web then reports under the new ID when
// the stream changed and under the old one when it did not; both belong to
// the same play (#73).
func TestStreamSwitchContinuesPlay(t *testing.T) {
	store, _, _ := newTestServer(t)
	movie := playbackMovie()
	movie.RunTimeTicks = 600000000
	catalog := &media.Catalog{ID: "movies", Items: []media.Item{movie, {ID: "other", RunTimeTicks: 100000000}}}
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
	playbackInfo := func(item, query string) string {
		t.Helper()
		w := request(h, "POST", "/Items/"+item+"/PlaybackInfo?"+query, "application/json", "{}", token)
		expectStatus(t, w, 200)
		var response struct{ PlaySessionId string }
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response.PlaySessionId
	}
	position := func() int64 { return store.Snapshot().User.Items["movie"].PositionTicks }
	expect := func(want int64, why string) {
		t.Helper()
		if got := position(); got != want {
			t.Fatalf("%s: position %d, want %d", why, got, want)
		}
	}

	first := playbackInfo("movie", "")
	report("", first, 0)
	report("/Progress", first, 10000000)
	// Another item or a play the server never issued does not continue it.
	unrelated := playbackInfo("other", "CurrentPlaySessionId="+first)
	stranger := playbackInfo("movie", "CurrentPlaySessionId=unknown")
	report("/Progress", unrelated, 15000000)
	report("/Progress", stranger, 15000000)
	expect(10000000, "unrelated play session")
	// Both the old and the new ID report for the play, also after a second
	// switch named by the first switch's ID.
	second := playbackInfo("movie", "MaxStreamingBitrate=420000&CurrentPlaySessionId="+first)
	report("/Progress", second, 20000000)
	expect(20000000, "progress under the new ID")
	report("/Progress", first, 25000000)
	expect(25000000, "progress under the old ID")
	third := playbackInfo("movie", "MaxStreamingBitrate=320000&CurrentPlaySessionId="+second)
	report("/Progress", third, 30000000)
	expect(30000000, "progress after a second switch")
	report("/Stopped", third, 40000000)
	if got := store.Snapshot().User.Items["movie"]; got.PositionTicks != 40000000 || got.PlayCount != 1 {
		t.Fatalf("after stop: %+v", got)
	}
	// A stopped play neither moves nor continues.
	report("/Progress", first, 5000000)
	fourth := playbackInfo("movie", "CurrentPlaySessionId="+third)
	report("/Progress", fourth, 50000000)
	expect(40000000, "stopped play")
	expectStatus(t, request(h, "POST", "/Items/movie/PlaybackInfo?CurrentPlaySessionId="+strings.Repeat("a", 129), "application/json", "{}", token), 400)
}
