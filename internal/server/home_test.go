package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/TokenCemetery/coach/internal/media"
)

// TestHomeSectionTitlesFollowLanguage covers #18: row titles follow
// X-Emby-Language for the chosen languages and fall back to English.
func TestHomeSectionTitlesFollowLanguage(t *testing.T) {
	store, _, _ := newTestServer(t)
	catalog := &media.Catalog{ID: "movies", Items: []media.Item{{ID: "m", Name: "Movie"}}}
	h := New(store, "test", nil, catalog).Handler()
	token := login(t, h)
	path := "/Users/" + store.Snapshot().User.ID + "/HomeSections"
	titles := func(query, header string) string {
		r := httptest.NewRequestWithContext(t.Context(), "GET", path+query, nil)
		r.Header.Set("X-Emby-Token", token)
		if header != "" {
			r.Header.Set("X-Emby-Language", header)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		expectStatus(t, w, 200)
		var sections []struct{ Name string }
		if err := json.Unmarshal(w.Body.Bytes(), &sections); err != nil {
			t.Fatal(err)
		}
		names := []string{}
		for _, s := range sections {
			names = append(names, s.Name)
		}
		return strings.Join(names, "|")
	}
	for _, tc := range []struct{ query, header, want string }{
		{"?X-Emby-Language=ru", "", "Мои медиаданные|Продолжение просмотра|Недавно добавленные фильмы"},
		{"?X-Emby-Language=es-MX", "", "Mis Contenidos|Continuar viendo|Ultimas Películas"},
		{"", "de", "Meine Medien|Weiterschauen|Neueste Filme"},
		{"?X-Emby-Language=ja", "", "My Media|Continue Watching|Latest Movies"},
		{"", "", "My Media|Continue Watching|Latest Movies"},
	} {
		if got := titles(tc.query, tc.header); got != tc.want {
			t.Fatalf("%q %q: got %s, want %s", tc.query, tc.header, got, tc.want)
		}
	}
}
