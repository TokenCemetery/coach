package server

import (
	"encoding/json"
	"reflect"
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
