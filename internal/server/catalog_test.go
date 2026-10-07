package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TokenCemetery/coach/internal/media"
)

func TestMovieCatalog(t *testing.T) {
	s, _, _ := newTestServer(t)
	catalog := &media.Catalog{ID: "movies", Items: []media.Item{
		{ID: "b", Name: "Beta", Container: "mkv", Modified: time.Unix(200, 0).UTC(), RunTimeTicks: 20000000, Streams: []media.Stream{{Type: "Video", Codec: "h264"}}},
		{ID: "a", Name: "Alpha", Container: "mp4", Modified: time.Unix(100, 0).UTC(), RunTimeTicks: 10000000, Streams: []media.Stream{{Type: "Video", Codec: "h264"}}},
	}}
	h := New(s, "test", nil, catalog).Handler()
	token := login(t, h)
	uid := s.Snapshot().User.ID
	base := "/emby/Users/" + uid
	page := func(path string) (ids []string, total int) {
		t.Helper()
		w := request(h, "GET", path, "", "", token)
		expectStatus(t, w, 200)
		var p struct {
			Items            []struct{ Id string }
			TotalRecordCount int
		}
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		if p.Items == nil {
			t.Fatal("Items must not be null")
		}
		for _, item := range p.Items {
			ids = append(ids, item.Id)
		}
		return ids, p.TotalRecordCount
	}
	for _, tc := range []struct {
		path, ids string
		total     int
	}{
		{base + "/Views", "movies", 1},
		{base + "/Items", "movies", 1},
		{base + "/Items?ParentId=movies", "a,b", 2},
		{"/Items?Recursive=true&IncludeItemTypes=Movie&Limit=1&StartIndex=1", "b", 2},
		{"/Items?ParentId=movies&SearchTerm=ALP", "a", 1},
		{"/Items?Recursive=true&SortBy=DateCreated&SortOrder=Descending", "b,a", 2},
		{"/Items?Recursive=true&SortBy=Runtime&SortOrder=Descending", "b,a", 2},
		{"/Items?Ids=b", "b", 1},
		{"/Items?Recursive=true&ExcludeItemIds=b", "a", 1},
		{"/Items?Recursive=true&ExcludeItemTypes=Movie", "", 0},
		{"/Items?Recursive=true&MediaTypes=Audio", "", 0},
		{"/Items?Recursive=true&Filters=IsPlayed", "", 0},
		{"/Items?Recursive=true&Filters=IsUnplayed", "a,b", 2},
		{"/Items?Recursive=true&IsFavorite=true", "", 0},
		{"/Items?ParentId=missing", "", 0},
		{"/Items?Recursive=true&StartIndex=99999", "", 2},
		{"/Items?Recursive=true&Limit=0", "", 2},
	} {
		ids, total := page(tc.path)
		if strings.Join(ids, ",") != tc.ids || total != tc.total {
			t.Errorf("%s: ids=%v total=%d", tc.path, ids, total)
		}
	}
	for _, path := range []string{base + "/Items/a", "/Items/a?UserId=" + uid, "/Items/movies", base + "/Items/Root", "/Items/Root"} {
		expectStatus(t, request(h, "GET", path, "", "", ""), 401)
		w := request(h, "GET", path, "", "", token)
		expectStatus(t, w, 200)
		if strings.Contains(w.Body.String(), `"Path"`) {
			t.Fatal("filesystem path exposed")
		}
	}
	var latest []struct{ Id string }
	w := request(h, "GET", base+"/Items/Latest?Limit=1", "", "", token)
	expectStatus(t, w, 200)
	if err := json.Unmarshal(w.Body.Bytes(), &latest); err != nil || len(latest) != 1 || latest[0].Id != "b" {
		t.Fatal("latest response is incorrect")
	}
	for _, query := range []string{"Limit=-1", "Limit=1001", "StartIndex=bad", "StartIndex=999999999999999999999", "Limit=1&limit=2", "Recursive=yes", "SortBy=Studio", "SortOrder=oops", "PersonIds=x", "Filters=IsResumable"} {
		expectStatus(t, request(h, "GET", "/Items?"+query, "", "", token), 400)
	}
	// Coach has no genres: a genre filter is accepted and matches nothing,
	// rather than returning the unfiltered catalogue (#78).
	w = request(h, "GET", "/Items?Recursive=true&Genres=Comedy", "", "", token)
	expectStatus(t, w, 200)
	if !strings.Contains(w.Body.String(), `"TotalRecordCount":0`) {
		t.Fatalf("genre filter: %s", w.Body.String())
	}
	expectStatus(t, request(h, "GET", "/Items/missing", "", "", token), 404)
	expectStatus(t, request(h, "GET", "/Users/other/Items/a", "", "", token), 403)
	expectStatus(t, request(h, "GET", "/Items/a?UserId=other", "", "", token), 403)
	// HEAD inherits GET routing without mutating the catalogue.
	r := httptest.NewRequestWithContext(t.Context(), http.MethodHead, "/Items/a", nil)
	r.Header.Set("X-Emby-Token", token)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	expectStatus(t, w, 200)
	if catalog.Items[0].ID != "b" {
		t.Fatal("query sorting mutated catalogue")
	}
}

// A library that appears with a rescan opens like one found at startup.
func TestRescannedLibraryOpens(t *testing.T) {
	store, _, _ := newTestServer(t)
	api := New(store, "test", nil, &media.Catalog{ID: "movies", Items: []media.Item{playbackMovie()}})
	h := api.Handler()
	token := login(t, h)
	next := &media.Catalog{ID: "movies", Items: []media.Item{playbackMovie()},
		Folders: []media.Item{{ID: "series", Kind: "Series", Name: "Show"}}}
	api.SetCatalog(next)
	for _, path := range []string{"/Items/" + next.SeriesLibraryID(), "/Users/" + store.Snapshot().User.ID + "/Items/" + next.SeriesLibraryID()} {
		expectStatus(t, request(h, "GET", path, "", "", token), 200)
	}
}

// Queries Emby Web sends from the Favorites tab (#76) and the "Add to
// playlist" dialog (#77).
func TestItemsQueriesFromEmbyWeb(t *testing.T) {
	store, _, _ := newTestServer(t)
	catalog := &media.Catalog{ID: "movies", Items: []media.Item{
		playbackMovie(),
		{ID: "episode", Kind: "Episode", Name: "Show S01E01", ParentID: "season", SeriesID: "series", SeasonID: "season", SeasonNumber: 1, EpisodeNumber: 1},
	}}
	catalog.Folders = []media.Item{
		{ID: "series", Kind: "Series", Name: "Show", ParentID: catalog.SeriesLibraryID()},
		{ID: "season", Kind: "Season", Name: "Season 1", ParentID: "series", SeriesID: "series", SeasonNumber: 1},
	}
	h := New(store, "test", nil, catalog).Handler()
	token := login(t, h)
	user := store.Snapshot().User.ID
	for types, want := range map[string]string{"movies": "movie", "tvshows": "episode,season,series", "music": "", "music,Movies": "movie"} {
		w := request(h, "GET", "/Users/"+user+"/Items?Recursive=true&SortBy=SortName&CollectionTypes="+types, "", "", token)
		expectStatus(t, w, 200)
		var page struct{ Items []struct{ Id string } }
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		ids := []string{}
		for _, item := range page.Items {
			ids = append(ids, item.Id)
		}
		slices.Sort(ids)
		if got := strings.Join(ids, ","); got != want {
			t.Fatalf("CollectionTypes=%s: %s, want %s", types, got, want)
		}
	}
	// "Add to playlist" lists the playlists the user can edit: none (#77).
	for query, want := range map[string]string{"CanEditItems=true": `"TotalRecordCount":0`, "CanEditItems=false": `"TotalRecordCount":4`} {
		w := request(h, "GET", "/Users/"+user+"/Items?Recursive=true&"+query, "", "", token)
		expectStatus(t, w, 200)
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("%s: %s", query, w.Body.String())
		}
	}
	expectStatus(t, request(h, "GET", "/Users/"+user+"/Items?CanEditItems=maybe", "", "", token), 400)
	// The same tab sorts favorite TV channels by channel number; Coach has
	// none, so the sort falls through to the next key.
	w := request(h, "GET", "/Users/"+user+"/Items?Recursive=true&IncludeItemTypes=TvChannel&SortBy=ChannelNumber,SortName", "", "", token)
	expectStatus(t, w, 200)
	if !strings.Contains(w.Body.String(), `"TotalRecordCount":0`) {
		t.Fatalf("TV channels: %s", w.Body.String())
	}
}

// Every sort Emby Web's library menus offer answers 200 (#79).
func TestLibraryMenuSorts(t *testing.T) {
	store, _, _ := newTestServer(t)
	day := func(d int) time.Time { return time.Date(2026, 1, d, 0, 0, 0, 0, time.UTC) }
	catalog := &media.Catalog{ID: "movies", Items: []media.Item{
		{ID: "big", Name: "Alpha", Container: "mkv", Size: 300, RunTimeTicks: 10000000, Added: day(1),
			Streams: []media.Stream{{Type: "Video", Codec: "hevc", Width: 3840, Height: 2160, AverageFrameRate: 24}}},
		{ID: "small", Name: "Beta", Container: "avi", Size: 100, RunTimeTicks: 10000000, Added: day(2),
			Streams: []media.Stream{{Type: "Video", Codec: "mpeg4", Width: 640, Height: 360, AverageFrameRate: 25}}},
		{ID: "mid", Name: "Gamma", Container: "mp4", Size: 200, RunTimeTicks: 10000000, Added: day(3),
			Streams: []media.Stream{{Type: "Video", Codec: "h264", Width: 1920, Height: 1080, AverageFrameRate: 30}}},
		{ID: "old", Kind: "Episode", Name: "Old S01E01", SeriesID: "old-series", SeasonID: "old-season", ParentID: "old-season", SeasonNumber: 1, EpisodeNumber: 1, Added: day(9)},
		{ID: "new", Kind: "Episode", Name: "New S01E01", SeriesID: "new-series", SeasonID: "new-season", ParentID: "new-season", SeasonNumber: 1, EpisodeNumber: 1, Added: day(5)},
	}}
	catalog.Folders = []media.Item{
		{ID: "old-series", Kind: "Series", Name: "A Show", ParentID: catalog.SeriesLibraryID()},
		{ID: "new-series", Kind: "Series", Name: "B Show", ParentID: catalog.SeriesLibraryID()},
	}
	h := New(store, "test", nil, catalog).Handler()
	token := login(t, h)
	user := store.Snapshot().User.ID
	order := func(query string) string {
		t.Helper()
		w := request(h, "GET", "/Users/"+user+"/Items?Recursive=true&"+query, "", "", token)
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
	// Watch "small" twice and the newer series' episode last.
	report := func(id, play string) {
		t.Helper()
		body := `{"ItemId":"` + id + `","PlaySessionId":"` + play + `","PositionTicks":0}`
		expectStatus(t, request(h, "POST", "/Sessions/Playing", "application/json", body, token), 204)
	}
	report("small", "p1")
	report("small", "p2")
	report("old", "p3")
	report("new", "p4")
	movies := "IncludeItemTypes=Movie&SortBy="
	for sort, want := range map[string]string{
		"TotalBitrate,SortName":                "small,mid,big",
		"VideoCodec,SortName":                  "mid,big,small",
		"Container,SortName":                   "small,big,mid",
		"Size,SortName":                        "small,mid,big",
		"Resolution,SortName":                  "small,mid,big",
		"Framerate,SortName":                   "big,small,mid",
		"PlayCount,SortName":                   "big,mid,small",
		"Runtime,SortName":                     "big,small,mid",
		"OfficialRating,SortName":              "big,small,mid",
		"ProductionYear,PremiereDate,SortName": "big,small,mid",
		"CommunityRating,SortName":             "big,small,mid",
		"CriticRating,SortName":                "big,small,mid",
		"Director,SortName":                    "big,small,mid",
	} {
		if got := order(movies + sort); got != want {
			t.Fatalf("SortBy=%s: %s, want %s", sort, got, want)
		}
	}
	if got := strings.Split(order(movies+"Random"), ","); len(got) != 3 || !slices.Contains(got, "big") || !slices.Contains(got, "small") || !slices.Contains(got, "mid") {
		t.Fatalf("Random: %v", got)
	}
	series := "IncludeItemTypes=Series&SortBy="
	for sort, want := range map[string]string{
		"DateLastContentAdded,SortName":    "new-series,old-series",
		"SeriesDatePlayed,SortName":        "old-series,new-series",
		"LastContentPremiereDate,SortName": "old-series,new-series",
	} {
		if got := order(series + sort); got != want {
			t.Fatalf("SortBy=%s: %s, want %s", sort, got, want)
		}
	}
}
