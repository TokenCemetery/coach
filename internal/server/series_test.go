package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/TokenCemetery/coach/internal/media"
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
