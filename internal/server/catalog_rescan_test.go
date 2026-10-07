package server

import (
	"testing"

	"github.com/TokenCemetery/coach/internal/media"
)

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
