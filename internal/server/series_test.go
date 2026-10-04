package server

import (
	"encoding/json"
	"net/http"
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
		// Play on an episode page builds the queue with this query.
		{"/Shows/show/Episodes?IsVirtualUnaired=false&IsMissing=false&Fields=PrimaryImageAspectRatio", "sp1,e1,e2"},
		{"/Shows/show/Episodes?IsMissing=true", ""},
		{"/Items?Recursive=true&IncludeItemTypes=Episode&IsVirtualUnaired=true", ""},
		// Series playback query sent by Emby Web 4.10.0.40.
		{"/Items?ParentId=show&Filters=IsNotFolder&Recursive=true&IsStandaloneSpecial=false&ExcludeLocationTypes=Virtual&CollapseBoxSetItems=false&SortBy=ParentIndexNumber,IndexNumber", "e1,e2"},
		{"/Items?ParentId=show&Filters=IsFolder&Recursive=true&IsStandaloneSpecial=true", ""},
		{"/Items?ParentId=show&Recursive=true&IsStandaloneSpecial=true", "sp1"},
		{"/Shows/show/Episodes?SeasonId=season&Limit=1&StartIndex=1", "e2"},
		{"/Items?Recursive=true&IncludeItemTypes=Episode&SearchTerm=Two", "e2"},
		{"/Users/" + store.Snapshot().User.ID + "/Sections/latestmedia_movies/Items", "movie"},
		// Premieres row of the TV Suggestions tab; Coach has no premiere dates.
		{"/Items?IncludeItemTypes=Episode&Recursive=true&SortBy=ProductionYear,PremiereDate,SortParentIndexNumber,SortIndexNumber&SortOrder=Descending,Descending,Ascending,Ascending&MinPremiereDate=2026-09-20T08:35:05.677Z&IsUnaired=false&ParentId=" + catalog.SeriesLibraryID() + "&Limit=12", ""},
		{"/Items?Recursive=true&IncludeItemTypes=Episode&IsUnaired=true", ""},
		{"/Items?Recursive=true&IncludeItemTypes=Episode&IsUnaired=false&SortBy=ParentIndexNumber,IndexNumber", "sp1,e1,e2"},
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
	expectStatus(t, request(h, "GET", "/Items?MinPremiereDate=yesterday", "", "", token), 400)
	expectStatus(t, request(h, "GET", "/Items?IsUnaired=maybe", "", "", token), 400)
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

func TestResumeIncludesNextUp(t *testing.T) {
	store, _, _ := newTestServer(t)
	catalog := &media.Catalog{ID: "movies"}
	episode := func(id, series string, number int) media.Item {
		return media.Item{ID: id, Name: id, Kind: "Episode", ParentID: series + "-s", SeasonID: series + "-s", SeriesID: series, SeriesName: series, SeasonNumber: 1, EpisodeNumber: number, RunTimeTicks: 100000000}
	}
	catalog.Items = []media.Item{
		{ID: "movie", Name: "Movie", RunTimeTicks: 100000000},
		episode("a1", "a", 1), episode("a2", "a", 2),
		episode("b1", "b", 1), episode("b2", "b", 2),
		episode("c1", "c", 1), episode("c2", "c", 2),
	}
	h := New(store, "test", nil, catalog).Handler()
	token := login(t, h)
	now := time.Now()
	if err := store.Change(token, func(d *state.Data, _ *state.Session) {
		d.User.Items = map[string]state.ItemState{
			"movie": {PositionTicks: 50000000, LastPlayed: now.Add(-30 * time.Minute)},
			"a1":    {Played: true, LastPlayed: now.Add(-time.Hour)},
			"b1":    {Played: true, LastPlayed: now.Add(-10 * time.Minute)},
			"c1":    {PositionTicks: 50000000, LastPlayed: now},
		}
	}); err != nil {
		t.Fatal(err)
	}
	base := "/Users/" + store.Snapshot().User.ID + "/Items/Resume?Recursive=true&MediaTypes=Video"
	for _, tc := range []struct{ query, ids string }{
		{"", "c1,movie"},
		{"&IncludeNextUp=false", "c1,movie"},
		// Next-up episodes are dated by when their series was last watched.
		{"&IncludeNextUp=true", "c1,b2,movie,a2"},
		{"&IncludeNextUp=true&IncludeItemTypes=Episode&ParentId=" + catalog.SeriesLibraryID(), "c1,b2,a2"},
		{"&IncludeNextUp=true&ParentId=a", "a2"},
		{"&IncludeNextUp=true&Limit=1&StartIndex=1", "b2"},
	} {
		w := request(h, "GET", base+tc.query, "", "", token)
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
			t.Fatalf("%s: got %v, want %s", tc.query, ids, tc.ids)
		}
	}
	expectStatus(t, request(h, "GET", base+"&IncludeNextUp=maybe", "", "", token), 400)
	// Emby Web fills the home row from the section, which advertises
	// IncludeNextUpInResume, not from Items/Resume.
	for _, tc := range []struct{ query, ids string }{{"", "c1,b2,movie,a2"}, {"?Limit=2", "c1,b2"}, {"?Limit=5000&StartIndex=0&Recursive=true&IsFolder=false", "c1,b2,movie,a2"}} {
		w := request(h, "GET", "/Users/"+store.Snapshot().User.ID+"/Sections/resume/Items"+tc.query, "", "", token)
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
			t.Fatalf("section%s: got %v, want %s", tc.query, ids, tc.ids)
		}
	}
	expectStatus(t, request(h, "GET", "/Users/"+store.Snapshot().User.ID+"/Sections/resume/Items?Limit=-1", "", "", token), 400)
	// "Remove from Continue Watching" on a next-up episode, which has no
	// position to clear, hides it until it is played again.
	section := "/Users/" + store.Snapshot().User.ID + "/Sections/resume/Items"
	expectStatus(t, request(h, "POST", "/Users/"+store.Snapshot().User.ID+"/Items/b2/HideFromResume?Hide=true", "", "", token), 200)
	for _, path := range []string{section, base + "&IncludeNextUp=true"} {
		if got := pageIDs(t, h, path, token); got != "c1,movie,a2" {
			t.Fatalf("%s after hide: got %s", path, got)
		}
	}
	expectStatus(t, request(h, "POST", "/Sessions/Playing", "application/json", `{"ItemId":"b2","PositionTicks":0}`, token), 204)
	if got := pageIDs(t, h, section, token); got != "b2,c1,movie,a2" {
		t.Fatalf("after replay: got %s", got)
	}
}

func pageIDs(t *testing.T, h http.Handler, path, token string) string {
	t.Helper()
	w := request(h, "GET", path, "", "", token)
	expectStatus(t, w, 200)
	var page struct{ Items []struct{ Id string } }
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, item := range page.Items {
		ids = append(ids, item.Id)
	}
	return strings.Join(ids, ",")
}

// TestLibraryTabRequests covers requests Emby Web sends from the series page
// and the library tabs (issue 47).
func TestLibraryTabRequests(t *testing.T) {
	store, _, _ := newTestServer(t)
	catalog := &media.Catalog{ID: "movies"}
	episode := func(id, series string, season, number int) media.Item {
		return media.Item{ID: id, Name: id, Kind: "Episode", ParentID: series + "-s", SeasonID: series + "-s", SeriesID: series, SeriesName: series, SeasonNumber: season, EpisodeNumber: number, Path: series + "/" + id + ".mp4"}
	}
	catalog.Items = []media.Item{
		{ID: "m1", Name: "Alpha", Path: "Movies/b-file.mp4"},
		{ID: "m2", Name: "Beta", Path: "Movies/a-file.mp4"},
		episode("b2", "Bravo", 1, 2), episode("b1", "Bravo", 1, 1),
		episode("a1", "Alpha Show", 2, 1), episode("a0", "Alpha Show", 0, 1),
	}
	h := New(store, "test", nil, catalog).Handler()
	token := login(t, h)
	user := "/Users/" + store.Snapshot().User.ID
	for _, id := range []string{"b2", "b1", "a1", "a0"} {
		expectStatus(t, request(h, "POST", user+"/FavoriteItems/"+id, "", "", token), 200)
	}
	for _, tc := range []struct{ path, ids string }{
		{user + "/Items?ParentId=Alpha Show&Recursive=true&IsFolder=false&IsSpecialEpisode=true&Limit=12", "a0"},
		{user + "/Items?ParentId=Alpha Show&Recursive=true&IsFolder=false&IsSpecialEpisode=false", "a1"},
		{user + "/Items?IncludeItemTypes=Episode&Filters=IsFavorite&Recursive=true&SortBy=SeriesSortName,ParentIndexNumber,IndexNumber,SortName&SortOrder=Ascending", "a0,a1,b1,b2"},
		{user + "/Items?ParentId=movies&StartIndex=0&SortBy=IsFolder,Filename&Limit=50", "m2,m1"},
	} {
		if got := pageIDs(t, h, strings.ReplaceAll(tc.path, " ", "%20"), token); got != tc.ids {
			t.Fatalf("%s: got %s, want %s", tc.path, got, tc.ids)
		}
	}
	if got := pageIDs(t, h, "/LiveTv/Programs?HasAired=false&LibrarySeriesId=Bravo&Limit=12", token); got != "" {
		t.Fatal("Live TV programs are not empty:", got)
	}
	w := request(h, "GET", "/Movies/Recommendations?ParentId=movies&categoryLimit=6&ItemLimit=12", "", "", token)
	expectStatus(t, w, 200)
	if strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatal("recommendations are not empty:", w.Body.String())
	}
	expectStatus(t, request(h, "GET", user+"/Items?SortBy=Random", "", "", token), 400)
}
