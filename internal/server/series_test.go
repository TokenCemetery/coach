package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/TokenCemetery/coach/internal/media"
	"github.com/TokenCemetery/coach/internal/state"
)

func TestSeriesCatalog(t *testing.T) {
	store, _, _ := newTestServer(t)
	catalog := &media.Catalog{ID: "movies"}
	catalog.Folders = []media.Item{
		{ID: "show", Name: "Show", Kind: "Series", ParentID: catalog.SeriesLibraryID()},
		{ID: "season", Name: "Season 1", Kind: "Season", ParentID: "show", SeriesID: "show", SeasonNumber: 1},
		{ID: "specials", Name: "Specials", Kind: "Season", ParentID: "show", SeriesID: "show"},
		{ID: "foreign", Name: "Other", Kind: "Season", ParentID: "other", SeriesID: "other"},
	}
	catalog.Items = []media.Item{
		{ID: "movie", Name: "Movie"},
		{ID: "e2", Name: "Episode Two", Kind: "Episode", ParentID: "season", SeasonID: "season", SeriesID: "show", SeriesName: "Show", SeasonNumber: 1, EpisodeNumber: 2, RunTimeTicks: 100000000},
		{ID: "e1", Name: "Episode One", Kind: "Episode", ParentID: "season", SeasonID: "season", SeriesID: "show", SeriesName: "Show", SeasonNumber: 1, EpisodeNumber: 1, RunTimeTicks: 100000000},
		{ID: "sp1", Name: "Special", Kind: "Episode", ParentID: "specials", SeasonID: "specials", SeriesID: "show", SeriesName: "Show", EpisodeNumber: 1, RunTimeTicks: 100000000},
	}
	h := New(store, "test", nil, catalog).Handler()
	token := login(t, h)
	for _, tc := range []struct{ path, ids string }{
		{"/Items?ParentId=movies&Recursive=true", "movie"},
		{"/Items?ParentId=" + catalog.SeriesLibraryID(), "show"},
		{"/Items?ParentId=show", "season,specials"},
		{"/Shows/show/Seasons", "specials,season"},
		{"/Shows/show/Seasons?IsSpecialSeason=false", "season"},
		{"/Shows/show/Seasons?IsSpecialSeason=true&ExcludeItemIds=other", "specials"},
		{"/Shows/show/Episodes", "sp1,e1,e2"},
		// Series playback query sent by Emby Web 4.10.0.40.
		{"/Items?ParentId=show&Filters=IsNotFolder&Recursive=true&IsStandaloneSpecial=false&ExcludeLocationTypes=Virtual&CollapseBoxSetItems=false&SortBy=ParentIndexNumber,IndexNumber", "e1,e2"},
		{"/Items?ParentId=show&Filters=IsFolder&Recursive=true&IsStandaloneSpecial=true", ""},
		{"/Items?ParentId=show&Recursive=true&IsStandaloneSpecial=true", "sp1"},
		{"/Shows/show/Episodes?SeasonId=season&Limit=1&StartIndex=1", "e2"},
		{"/Items?Recursive=true&IncludeItemTypes=Episode&SearchTerm=Two", "e2"},
		{"/Users/" + store.Snapshot().User.ID + "/Sections/latestmedia_movies/Items", "movie"},
	} {
		w := request(h, "GET", tc.path, "", "", token)
		expectStatus(t, w, 200)
		var page struct{ Items []struct{ Id string } }
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		ids := []string{}
		for _, item := range page.Items {
			ids = append(ids, item.Id)
		}
		if strings.Join(ids, ",") != tc.ids {
			t.Fatalf("%s: %v", tc.path, ids)
		}
	}
	for _, id := range []string{"show", "season", "e1", catalog.SeriesLibraryID()} {
		expectStatus(t, request(h, "GET", "/Items/"+id, "", "", token), 200)
	}
	for _, path := range []string{"/Shows/movie/Seasons", "/Shows/show/Episodes?SeasonId=foreign", "/Items/show/PlaybackInfo", "/Videos/season/stream.mp4"} {
		expectStatus(t, request(h, "GET", path, "", "", token), 404)
	}
	expectStatus(t, request(h, "GET", "/Shows/show/Seasons", "", "", ""), 401)
	expectStatus(t, request(h, "GET", "/Shows/show/Seasons?IsSpecialSeason=maybe", "", "", token), 400)
	expectStatus(t, request(h, "GET", "/Shows/show/Episodes?IsSpecialSeason=false", "", "", token), 400)
	expectStatus(t, request(h, "GET", "/Items?ExcludeLocationTypes=FileSystem", "", "", token), 400)
	expectStatus(t, request(h, "GET", "/Items?IsStandaloneSpecial=yes", "", "", token), 400)
	expectStatus(t, request(h, "GET", "/Shows/show/Episodes?UserId=other", "", "", token), 403)
	expectStatus(t, request(h, "POST", "/Sessions/Playing", "application/json", `{"ItemId":"season"}`, token), 404)
	w := request(h, "GET", "/Items/e1", "", "", token)
	var episode map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &episode); err != nil {
		t.Fatal(err)
	}
	if episode["Type"] != "Episode" || episode["SeasonId"] != "season" || episode["SeriesId"] != "show" || episode["IndexNumber"] != float64(1) {
		t.Fatal("episode DTO")
	}
}

func TestNextUp(t *testing.T) {
	store, _, _ := newTestServer(t)
	catalog := &media.Catalog{ID: "movies"}
	episode := func(id, series string, season, number int) media.Item {
		return media.Item{ID: id, Name: id, Kind: "Episode", ParentID: series + "-s", SeasonID: series + "-s", SeriesID: series, SeriesName: series, SeasonNumber: season, EpisodeNumber: number, RunTimeTicks: 100000000}
	}
	catalog.Items = []media.Item{
		{ID: "movie", Name: "Movie"},
		episode("a2", "a", 1, 2), episode("a1", "a", 1, 1), episode("a0", "a", 0, 1),
		episode("b1", "b", 1, 1), episode("b2", "b", 1, 2), episode("b3", "b", 2, 1),
		episode("c1", "c", 1, 1), episode("c2", "c", 1, 2),
		episode("d1", "d", 1, 1),
	}
	h := New(store, "test", nil, catalog).Handler()
	token := login(t, h)
	now := time.Now()
	// a: first episode watched earlier; b: an episode skipped, then the second watched
	// most recently; c: never finished; d: finished with nothing after it.
	if err := store.Change(token, func(d *state.Data, _ *state.Session) {
		d.User.Items = map[string]state.ItemState{
			"a1": {Played: true, LastPlayed: now.Add(-time.Hour)},
			"a0": {Played: true, LastPlayed: now},
			"b2": {Played: true, LastPlayed: now.Add(-time.Minute)},
			"c1": {PositionTicks: 50000000, LastPlayed: now},
			"d1": {Played: true, LastPlayed: now},
		}
	}); err != nil {
		t.Fatal(err)
	}
	user := store.Snapshot().User.ID
	for _, tc := range []struct{ path, ids string }{
		// a0 is a special and is ignored, so b was watched more recently than a.
		{"/Shows/NextUp?UserId=" + user + "&LegacyNextUp=true&Fields=PrimaryImageAspectRatio&ImageTypeLimit=1", "b3,a2"},
		{"/Shows/NextUp?SeriesId=b", "b3"},
		{"/Shows/NextUp?ParentId=a", "a2"},
		{"/Shows/NextUp?ParentId=" + catalog.SeriesLibraryID() + "&StartIndex=1&Limit=1", "a2"},
		{"/Shows/NextUp?ParentId=movies", ""},
	} {
		w := request(h, "GET", tc.path, "", "", token)
		expectStatus(t, w, 200)
		var page struct {
			Items            []struct{ Id string }
			TotalRecordCount int
		}
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		ids := []string{}
		for _, item := range page.Items {
			ids = append(ids, item.Id)
		}
		if strings.Join(ids, ",") != tc.ids {
			t.Fatalf("%s: %v", tc.path, ids)
		}
	}
	expectStatus(t, request(h, "GET", "/Shows/NextUp", "", "", ""), 401)
	expectStatus(t, request(h, "GET", "/Shows/NextUp?Limit=-1", "", "", token), 400)
	expectStatus(t, request(h, "GET", "/Shows/NextUp?SortBy=Name", "", "", token), 400)
	expectStatus(t, request(h, "GET", "/Shows/NextUp?UserId=other", "", "", token), 403)
}
