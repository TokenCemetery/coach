package server

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/TokenCemetery/coach/internal/media"
)

func TestSearchItemTypes(t *testing.T) {
	store, _, _ := newTestServer(t)
	catalog := &media.Catalog{ID: "movies", Items: []media.Item{
		{ID: "a", Name: "Sample Movie"},
		{ID: "b", Name: "Sample Episode", Kind: "Episode"},
		{ID: "c", Name: "Sample Other Movie"},
	}}
	h := New(store, "test", nil, catalog).Handler()
	token := login(t, h)
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"SearchTerm=sample&GroupProgramsBySeries=true&IncludeSearchTypes=true", []string{"Episode", "Movie"}},
		{"SearchTerm=sample&StartIndex=100&Limit=0", []string{"Episode", "Movie"}},
		{"SearchTerm=movie", []string{"Movie"}},
		{"IncludeItemTypes=Episode", []string{"Episode"}},
		{"ExcludeItemTypes=Episode", []string{"Movie"}},
		{"SearchTerm=absent", []string{}},
	} {
		w := request(h, "GET", "/emby/ItemTypes?"+tc.query, "", "", token)
		expectStatus(t, w, 200)
		var result struct {
			Items            []struct{ Name string }
			TotalRecordCount int
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		names := make([]string, 0, len(result.Items))
		for _, item := range result.Items {
			names = append(names, item.Name)
		}
		if !reflect.DeepEqual(names, tc.want) || result.TotalRecordCount != len(tc.want) {
			t.Fatalf("%s: got %v (%d), want %v", tc.query, names, result.TotalRecordCount, tc.want)
		}
	}
	expectStatus(t, request(h, "GET", "/ItemTypes", "", "", ""), 401)
	for _, endpoint := range []string{"/ItemTypes", "/Items"} {
		for _, key := range []string{"GroupProgramsBySeries", "IncludeSearchTypes"} {
			expectStatus(t, request(h, "GET", endpoint+"?"+key+"=invalid", "", "", token), 400)
		}
	}
}

func TestSearchHistory(t *testing.T) {
	store, _, _ := newTestServer(t)
	catalog := &media.Catalog{ID: "movies", Items: []media.Item{
		{ID: "a", Name: "Alpha"}, {ID: "b", Name: "Beta"}, {ID: "c", Name: "Gamma"},
	}}
	h := New(store, "test", nil, catalog).Handler()
	token := login(t, h)
	base := "/Users/" + store.Snapshot().User.ID
	report := func(body string, status int) {
		t.Helper()
		expectStatus(t, request(h, "POST", base+"/SearchedItems", "application/json", body, token), status)
	}
	ids := func(path string) string {
		t.Helper()
		w := request(h, "GET", base+path, "", "", token)
		expectStatus(t, w, 200)
		var page struct{ Items []struct{ Id string } }
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		list := []string{}
		for _, item := range page.Items {
			list = append(list, item.Id)
		}
		return strings.Join(list, ",")
	}
	report(`{"Ids":["b"],"WasSearched":true}`, 204)
	report(`{"Ids":["c"],"WasSearched":true}`, 204)
	// Queries sent by Emby Web 4.10.0.40 search.js and searchfields.js.
	if got := ids("/Items?Recursive=true&ImageTypeLimit=1&WasSearched=true&SortBy=DateLastSearched&SortOrder=Descending"); got != "c,b" {
		t.Fatal(got)
	}
	if got := ids("/Items?SortBy=DateLastSearched,SortName&SortOrder=Descending&Limit=20&Recursive=true&EnableTotalRecordCount=false&WasSearched=true"); got != "c,b" {
		t.Fatal(got)
	}
	report(`{"Ids":["c"],"WasSearched":false}`, 204)
	if got := ids("/Items?Recursive=true&WasSearched=true"); got != "b" {
		t.Fatal(got)
	}
	if got := ids("/Items?Recursive=true&WasSearched=false"); got != "a,c" {
		t.Fatal(got)
	}
	if _, kept := store.Snapshot().User.Items["c"]; kept {
		t.Fatal("cleared search state was kept")
	}
	report(`{"Ids":["missing"],"WasSearched":true}`, 404)
	for _, body := range []string{`{"Ids":[],"WasSearched":true}`, `{"Ids":["a"]}`, `{"Ids":null,"WasSearched":true}`} {
		report(body, 400)
	}
	expectStatus(t, request(h, "GET", base+"/Items?WasSearched=maybe", "", "", token), 400)
	expectStatus(t, request(h, "POST", "/Users/other/SearchedItems", "application/json", `{"Ids":["a"],"WasSearched":true}`, token), 403)
}
