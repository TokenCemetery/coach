package server

import (
	"encoding/json"
	"testing"

	"github.com/TokenCemetery/coach/internal/media"
)

func TestCatalogSavedStateFilters(t *testing.T) {
	store, _, _ := newTestServer(t)
	catalog := &media.Catalog{ID: "movies", Items: []media.Item{
		{ID: "a", Name: "Alpha", RunTimeTicks: 100000000},
		{ID: "b", Name: "Beta", RunTimeTicks: 100000000},
	}}
	h := New(store, "test", nil, catalog).Handler()
	token := login(t, h)
	base := "/Users/" + store.Snapshot().User.ID
	for _, endpoint := range []string{"/PlayedItems/a", "/FavoriteItems/a"} {
		expectStatus(t, request(h, "POST", base+endpoint, "", "", token), 200)
	}
	for _, tc := range []struct{ query, id string }{
		{"IsPlayed=true", "a"}, {"IsPlayed=false", "b"},
		{"IsFavorite=true", "a"}, {"IsFavorite=false", "b"},
		{"Filters=IsPlayed", "a"}, {"Filters=IsUnplayed", "b"},
		{"Filters=IsFavorite", "a"}, {"Filters=IsFavorite,IsUnplayed", ""},
	} {
		w := request(h, "GET", base+"/Items?Recursive=true&"+tc.query, "", "", token)
		expectStatus(t, w, 200)
		var result struct {
			Items            []struct{ Id string }
			TotalRecordCount int
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if tc.id == "" {
			if result.TotalRecordCount != 0 {
				t.Fatal(tc.query, result)
			}
		} else if result.TotalRecordCount != 1 || result.Items[0].Id != tc.id {
			t.Fatal(tc.query, result)
		}
	}
	w := request(h, "GET", base+"/Sections/latestmedia_movies/Items", "", "", token)
	expectStatus(t, w, 200)
	var latest struct{ Items []struct{ Id string } }
	if err := json.Unmarshal(w.Body.Bytes(), &latest); err != nil || len(latest.Items) != 1 || latest.Items[0].Id != "b" {
		t.Fatal("latest includes played item")
	}
	for _, id := range []string{"a", "b"} {
		expectStatus(t, request(h, "POST", "/Sessions/Playing/Progress", "application/json", `{"ItemId":"`+id+`","PositionTicks":25000000}`, token), 204)
	}
	for _, endpoint := range []string{"/Sessions/Playing/Progress", "/Sessions/Playing/Stopped"} {
		expectStatus(t, request(h, "POST", endpoint, "application/json", `{"ItemId":"a"}`, token), 204)
		if store.Snapshot().User.Items["a"].PositionTicks != 25000000 {
			t.Fatal("missing position reset resume")
		}
		expectStatus(t, request(h, "POST", endpoint, "application/json", `{"ItemId":"a","PositionTicks":-1}`, token), 400)
	}
	w = request(h, "GET", base+"/Items/Resume?StartIndex=1&Limit=1&SortBy=Name", "", "", token)
	expectStatus(t, w, 200)
	var page struct {
		Items            []struct{ Id string }
		TotalRecordCount int
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || page.TotalRecordCount != 2 || len(page.Items) != 1 || page.Items[0].Id != "b" {
		t.Fatal("resume pagination", w.Body.String())
	}
	expectStatus(t, request(h, "GET", base+"/Items/Resume?Limit=-1", "", "", token), 400)
}
