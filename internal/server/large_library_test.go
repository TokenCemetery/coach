package server

import (
	"strconv"
	"testing"
	"time"

	"github.com/TokenCemetery/coach/internal/media"
	"github.com/TokenCemetery/coach/internal/state"
)

// largeLibrary has 1000 series of 2 seasons with 10 episodes each: 20 000
// episodes, the order of a large NAS TV library.
func largeLibrary() *media.Catalog {
	catalog := &media.Catalog{ID: "movies"}
	for s := range 1000 {
		series := "series-" + strconv.Itoa(s)
		catalog.Folders = append(catalog.Folders, media.Item{ID: series, Name: series, Kind: "Series", ParentID: catalog.SeriesLibraryID()})
		for season := 1; season <= 2; season++ {
			seasonID := series + "-s" + strconv.Itoa(season)
			catalog.Folders = append(catalog.Folders, media.Item{ID: seasonID, Name: "Season", Kind: "Season", ParentID: series, SeriesID: series, SeasonNumber: season})
			for e := 1; e <= 10; e++ {
				catalog.Items = append(catalog.Items, media.Item{ID: seasonID + "-e" + strconv.Itoa(e), Name: "Episode", Kind: "Episode",
					ParentID: seasonID, SeasonID: seasonID, SeriesID: series, SeasonNumber: season, EpisodeNumber: e, RunTimeTicks: 10000000})
			}
		}
	}
	return catalog
}

func BenchmarkSeriesPage(b *testing.B) {
	store, _, _ := newTestServer(b)
	catalog := largeLibrary()
	h := New(store, "test", nil, catalog).Handler()
	token := login(b, h)
	path := "/Users/" + store.Snapshot().User.ID + "/Items?ParentId=" + catalog.SeriesLibraryID() + "&IncludeItemTypes=Series&Recursive=true&Limit=100&Filters=IsUnplayed"
	for b.Loop() {
		if w := request(h, "GET", path, "", "", token); w.Code != 200 {
			b.Fatal(w.Code)
		}
	}
}

// BenchmarkScreens times the requests Emby Web makes for the home screen,
// search and an item page, with 300 series started.
func BenchmarkScreens(b *testing.B) {
	store, _, _ := newTestServer(b)
	catalog := largeLibrary()
	h := New(store, "test", nil, catalog).Handler()
	token := login(b, h)
	uid := store.Snapshot().User.ID
	if err := store.Change(token, func(d *state.Data, _ *state.Session) {
		d.User.Items = map[string]state.ItemState{}
		for s := range 300 {
			d.User.Items["series-"+strconv.Itoa(s)+"-s1-e1"] = state.ItemState{Played: true, LastPlayed: time.Now().Add(-time.Duration(s) * time.Minute)}
		}
	}); err != nil {
		b.Fatal(err)
	}
	for _, tc := range []struct{ name, path string }{
		{"NextUp", "/Shows/NextUp?UserId=" + uid + "&Limit=24"},
		{"ResumeSection", "/Users/" + uid + "/Sections/resume/Items?Limit=12"},
		{"Latest", "/Users/" + uid + "/Items/Latest?ParentId=" + catalog.SeriesLibraryID() + "&Limit=16"},
		{"Search", "/Users/" + uid + "/Items?SearchTerm=series-99&Recursive=true&Limit=24"},
		{"Item", "/Users/" + uid + "/Items/series-500"},
		{"Seasons", "/Shows/series-500/Seasons?UserId=" + uid},
		{"Episodes", "/Shows/series-500/Episodes?UserId=" + uid + "&SeasonId=series-500-s1"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			for b.Loop() {
				if w := request(h, "GET", tc.path, "", "", token); w.Code != 200 {
					b.Fatalf("%s: %d %s", tc.path, w.Code, w.Body.String())
				}
			}
		})
	}
}
