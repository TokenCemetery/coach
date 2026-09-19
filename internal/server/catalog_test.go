package server

import (
	"encoding/json"
	"net/http/httptest"
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
	for _, query := range []string{"Limit=-1", "Limit=1001", "StartIndex=bad", "StartIndex=999999999999999999999", "Limit=1&limit=2", "Recursive=yes", "SortBy=Random", "SortOrder=oops", "Genres=Comedy", "Filters=IsResumable"} {
		expectStatus(t, request(h, "GET", "/Items?"+query, "", "", token), 400)
	}
	expectStatus(t, request(h, "GET", "/Items/missing", "", "", token), 404)
	expectStatus(t, request(h, "GET", "/Users/other/Items/a", "", "", token), 403)
	expectStatus(t, request(h, "GET", "/Items/a?UserId=other", "", "", token), 403)
	// HEAD inherits GET routing without mutating the catalogue.
	r := httptest.NewRequest("HEAD", "/Items/a", nil)
	r.Header.Set("X-Emby-Token", token)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	expectStatus(t, w, 200)
	if catalog.Items[0].ID != "b" {
		t.Fatal("query sorting mutated catalogue")
	}
}
