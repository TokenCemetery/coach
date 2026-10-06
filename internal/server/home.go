package server

import (
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/TokenCemetery/coach/internal/media"
	"github.com/TokenCemetery/coach/internal/state"
)

// homeSection mirrors the section descriptor Emby returns from
// /Users/{id}/HomeSections. The client reads every key below without a guard,
// so all of them are emitted even when empty.
func homeSection(id, name, sectionType string, monitor []string, cardSizeOffset int) object {
	return object{
		"Name":                  name,
		"Id":                    id,
		"SectionType":           sectionType,
		"Monitor":               monitor,
		"ItemTypes":             []string{},
		"ExcludedFolders":       []string{},
		"CardSizeOffset":        cardSizeOffset,
		"IncludeNextUpInResume": true,
		"Query": object{
			"StudioIds":       []string{},
			"TagIds":          []string{},
			"GenreIds":        []string{},
			"CollectionTypes": []string{},
		},
	}
}

// homeSections describes the home screen Coach can actually populate: the
// library tiles, the resume row, and one latest row per library. Titles come
// from locales/*.yaml by X-Emby-Language (#18). Sections for
// media Coach does not serve yet (audio, Live TV) are not advertised.
func (s *Server) homeSections(serverID, language string) []object {
	sections := []object{
		homeSection("smalllibrarytiles", translate(language, "HeaderMyMedia"), "userviews", []string{}, -1),
		homeSection("resume", translate(language, "HeaderContinueWatching"), "resume", []string{"videoplayback", "markplayed"}, 0),
	}
	for _, id := range s.libraryIDs() {
		name, collectionType := translate(language, "HeaderLatestMovies"), "movies"
		if id == s.catalog().SeriesLibraryID() {
			name, collectionType = translate(language, "HeaderLatestEpisodes"), "tvshows"
		}
		latest := homeSection("latestmedia_"+id, name, "latestmedia",
			[]string{"markplayed", "videoplayback"}, 0)
		latest["CollectionType"] = collectionType
		latest["ParentItem"] = s.collectionDTO(id, serverID, false)
		latest["ParentId"] = id
		latest["Query"] = object{
			"StudioIds":       []string{},
			"TagIds":          []string{},
			"GenreIds":        []string{},
			"CollectionTypes": []string{},
			"IsPlayed":        false,
		}
		sections = append(sections, latest)
	}
	return sections
}

// sectionItems answers /Users/{id}/Sections/{section}/Items. Emby Web does not
// build a query per home row itself: it asks the server to fill each section it
// was given, so every section id advertised by homeSections must be answerable.
func (s *Server) sectionItems(w http.ResponseWriter, r *http.Request, token string) {
	catalog := s.catalog()
	section := strings.ToLower(r.PathValue("section"))
	limit := 12
	if text := r.URL.Query().Get("Limit"); text != "" {
		v, err := strconv.Atoi(text)
		if err != nil || v < 0 {
			fail(w, 400, "InvalidPagination")
			return
		}
		// The card menu asks for the whole row with Limit=5000 to offer
		// "play from here"; a row never needs more than 1000 items.
		limit = min(v, 1000)
	}
	snapshot := s.store.Snapshot()
	serverID := snapshot.ServerID
	items := []object{}
	switch {
	case section == "smalllibrarytiles":
		for _, id := range s.libraryIDs() {
			items = append(items, s.collectionDTO(id, serverID, false))
		}
	case section == "resume":
		items = s.resumeItems(limit, token)
	case section == "resumeaudio":
		// Coach serves no audio library, so this row is always empty.
	case strings.HasPrefix(section, "latestmedia_") && slices.Contains(s.libraryIDs(), strings.TrimPrefix(section, "latestmedia_")):
		series := strings.TrimPrefix(section, "latestmedia_") == catalog.SeriesLibraryID()
		latest := make([]*media.Item, 0, len(catalog.Items))
		for i := range catalog.Items {
			if !snapshot.User.Items[catalog.Items[i].ID].Played && (catalog.Items[i].Type() == "Episode") == series {
				latest = append(latest, &catalog.Items[i])
			}
		}
		slices.SortFunc(latest, func(a, b *media.Item) int {
			if c := b.Modified.Compare(a.Modified); c != 0 {
				return c
			}
			return strings.Compare(a.ID, b.ID)
		})
		for _, item := range latest[:min(limit, len(latest))] {
			items = append(items, s.movieDTO(*item, serverID, snapshot.User.Items, token))
		}
	default:
		fail(w, 404, "NotFound")
		return
	}
	if len(items) > limit {
		items = items[:limit]
	}
	respond(w, 200, object{"Items": items, "TotalRecordCount": len(items)})
}

func (s *Server) homeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /users/{user}/homesections", s.protect(func(w http.ResponseWriter, r *http.Request, token string, session state.Session) {
		respond(w, 200, s.homeSections(s.store.Snapshot().ServerID, value(r, "X-Emby-Language")))
	}))
	mux.HandleFunc("GET /users/{user}/sections/{section}/items", s.protect(func(w http.ResponseWriter, r *http.Request, token string, session state.Session) {
		s.sectionItems(w, r, token)
	}))
}
