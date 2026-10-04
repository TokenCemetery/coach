package media

import (
	"strings"
	"testing"
)

func TestCatalogIndex(t *testing.T) {
	c := &Catalog{ID: "movies", Items: []Item{
		{ID: "m"},
		{ID: "e1", Kind: "Episode", ParentID: "s1", SeasonID: "s1", SeriesID: "show"},
		{ID: "e2", Kind: "Episode", ParentID: "s2", SeasonID: "s2", SeriesID: "show"},
	}, Folders: []Item{
		{ID: "show", Kind: "Series", ParentID: tvLibrary},
		{ID: "s1", Kind: "Season", ParentID: "show", SeriesID: "show"},
		{ID: "s2", Kind: "Season", ParentID: "show", SeriesID: "show"},
	}}
	ids := func(items []Item) string {
		out := []string{}
		for _, item := range items {
			out = append(out, item.ID)
		}
		return strings.Join(out, ",")
	}
	if got := ids(c.Episodes("show")); got != "e1,e2" {
		t.Fatalf("series episodes: %s", got)
	}
	if got := ids(c.Episodes("s2")); got != "e2" {
		t.Fatalf("season episodes: %s", got)
	}
	for id, want := range map[string]int{"movies": 1, "show": 2, "s1": 1, tvLibrary: 1, "e1": 0} {
		if got := c.ChildCount(id); got != want {
			t.Fatalf("children of %s: %d, want %d", id, got, want)
		}
	}
}

const tvLibrary = "tv-library"
