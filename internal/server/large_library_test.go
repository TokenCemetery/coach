package server

import (
	"strconv"
	"testing"

	"github.com/TokenCemetery/coach/internal/media"
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
