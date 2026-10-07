package server

import (
	"cmp"
	"errors"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/TokenCemetery/coach/internal/media"
	"github.com/TokenCemetery/coach/internal/state"
)

// rootID is the parent of every library. Emby uses a numeric string here; Coach
// keeps its own opaque value because clients treat item IDs as opaque.
const rootID = "root"

func catalogQuery(r *http.Request) (map[string]string, error) {
	query := map[string]string{}
	for key, vv := range r.URL.Query() {
		key = strings.ToLower(key)
		for _, v := range vv {
			if old, ok := query[key]; ok && old != v {
				return nil, errors.New("conflicting query parameters")
			}
			query[key] = v
		}
	}
	return query, nil
}

// boolRank orders false before true.
func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

func member(list, value string) bool {
	for _, item := range strings.Split(list, ",") {
		if strings.EqualFold(strings.TrimSpace(item), value) {
			return true
		}
	}
	return false
}

func (s *Server) catalogRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /users/{user}/views", s.protect(func(w http.ResponseWriter, r *http.Request, token string, session state.Session) {
		items := []any{}
		serverID := s.store.Snapshot().ServerID
		for _, id := range s.libraryIDs() {
			items = append(items, s.collectionDTO(id, serverID, false))
		}
		respond(w, 200, object{"Items": items, "TotalRecordCount": len(items)})
	}))
	for _, path := range []string{"/items/root", "/users/{user}/items/root"} {
		mux.HandleFunc("GET "+path, s.protect(func(w http.ResponseWriter, r *http.Request, token string, session state.Session) {
			respond(w, 200, s.rootFolderDTO(s.store.Snapshot().ServerID))
		}))
	}
	for _, path := range []string{"/items/{item}", "/users/{user}/items/{item}"} {
		mux.HandleFunc("GET "+path, s.protect(func(w http.ResponseWriter, r *http.Request, token string, session state.Session) {
			id := r.PathValue("item")
			// A rescan replaces the catalog, so each request reads it (#75).
			if catalog := s.catalog(); catalog != nil {
				snapshot := s.store.Snapshot()
				if id == catalog.ID || (len(catalog.Folders) > 0 && id == catalog.SeriesLibraryID()) {
					respond(w, 200, s.collectionDTO(id, snapshot.ServerID, true))
					return
				}
				if item, found := s.findItem(id); found {
					respond(w, 200, s.movieDTO(item, snapshot.ServerID, snapshot.User.Items, token))
					return
				}
			}
			fail(w, 404, "NotFound")
		}))
	}
	for _, path := range []string{"/items", "/users/{user}/items", "/users/{user}/items/latest"} {
		latest := strings.HasSuffix(path, "/latest")
		mux.HandleFunc("GET "+path, s.protect(func(w http.ResponseWriter, r *http.Request, token string, session state.Session) {
			s.listItems(w, r, latest, false)
		}))
	}
	mux.HandleFunc("POST /users/{user}/searcheditems", s.protect(s.reportSearched))
	// The "Clear" button on the search page; Emby Web sends no body.
	mux.HandleFunc("POST /users/{user}/recentlysearched/delete", s.protect(func(w http.ResponseWriter, r *http.Request, token string, session state.Session) {
		s.changed(w, s.store.Change(token, func(d *state.Data, _ *state.Session) {
			for id, st := range d.User.Items {
				st.LastSearched = time.Time{}
				if st == (state.ItemState{}) {
					delete(d.User.Items, id)
				} else {
					d.User.Items[id] = st
				}
			}
		}))
	}))
	s.extrasRoutes(mux)
	s.seriesRoutes(mux)
	s.imageRoutes(mux)
	s.userImageRoutes(mux)
	mux.HandleFunc("GET /itemtypes", s.protect(func(w http.ResponseWriter, r *http.Request, token string, session state.Session) {
		s.listItems(w, r, false, false)
	}))
}

// extrasRoutes answers the companion requests Emby Web makes on an item page.
// Coach has none of this content, so each returns an empty result in the shape
// the reference server uses; a 404 here leaves the page in an error state.
func (s *Server) extrasRoutes(mux *http.ServeMux) {
	known := func(r *http.Request) bool {
		id := r.PathValue("item")
		_, found := s.findItem(id)
		return found || slices.Contains(s.libraryIDs(), id)
	}
	emptyPage := func(w http.ResponseWriter, r *http.Request, token string, session state.Session) {
		if !known(r) {
			fail(w, 404, "NotFound")
			return
		}
		respond(w, 200, object{"Items": []any{}, "TotalRecordCount": 0})
	}
	emptyArray := func(w http.ResponseWriter, r *http.Request, token string, session state.Session) {
		if !known(r) {
			fail(w, 404, "NotFound")
			return
		}
		respond(w, 200, []any{})
	}
	for _, path := range []string{"/users/{user}/items/{item}/intros", "/items/{item}/similar"} {
		mux.HandleFunc("GET "+path, s.protect(emptyPage))
	}
	for _, path := range []string{"/users/{user}/items/{item}/localtrailers", "/users/{user}/items/{item}/specialfeatures", "/items/{item}/specialfeatures"} {
		mux.HandleFunc("GET "+path, s.protect(emptyArray))
	}
	mux.HandleFunc("GET /items/{item}/thememedia", s.protect(func(w http.ResponseWriter, r *http.Request, token string, session state.Session) {
		if !known(r) {
			fail(w, 404, "NotFound")
			return
		}
		empty := object{"Items": []any{}, "TotalRecordCount": 0, "OwnerId": r.PathValue("item")}
		respond(w, 200, object{"ThemeVideosResult": empty, "ThemeSongsResult": empty, "SoundtrackSongsResult": empty})
	}))
	// Coach generates no seek-bar previews. Emby Web's player re-requests the
	// set after every failure, so an empty set is answered instead of 404.
	mux.HandleFunc("GET /items/{item}/thumbnailset", s.protect(func(w http.ResponseWriter, r *http.Request, token string, session state.Session) {
		if !known(r) {
			fail(w, 404, "NotFound")
			return
		}
		respond(w, 200, object{"Thumbnails": []any{}})
	}))
}

// reportSearched records items the user opened from search results; Emby Web
// lists them on the empty search page as "Recently searched".
func (s *Server) reportSearched(w http.ResponseWriter, r *http.Request, token string, session state.Session) {
	var report struct {
		Ids         []string
		WasSearched *bool
	}
	if decodeJSON(w, r, &report) != nil || len(report.Ids) == 0 || len(report.Ids) > 100 || report.WasSearched == nil {
		fail(w, 400, "InvalidRequest")
		return
	}
	for _, id := range report.Ids {
		if _, found := s.findItem(id); !found {
			fail(w, 404, "NotFound")
			return
		}
	}
	now := time.Now().UTC()
	for _, id := range report.Ids {
		err := s.store.SetItem(token, id, func(st *state.ItemState) {
			if *report.WasSearched {
				st.LastSearched = now
			} else {
				st.LastSearched = time.Time{}
			}
		})
		if err != nil {
			s.changed(w, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listItems(w http.ResponseWriter, r *http.Request, latest, resume bool) {
	catalog := s.catalog()
	token, _ := requestToken(r) // Callers have already authenticated this request.
	typesOnly := r.URL.Path == "/itemtypes"
	query, err := catalogQuery(r)
	if err != nil {
		fail(w, 400, "InvalidQuery")
		return
	}
	// Reject unsupported filters rather than returning a misleading unfiltered catalogue.
	for key := range query {
		switch key {
		case "parentid", "recursive", "searchterm", "includeitemtypes", "excludeitemtypes", "mediatypes", "ids", "excludeitemids",
			"startindex", "limit", "sortby", "sortorder", "isfolder", "isplayed", "isfavorite", "filters",
			"fields", "enableimages", "enableimagetypes", "imagetypelimit", "enableuserdata", "enabletotalrecordcount", "groupitems",
			"groupprogramsbyseries", "includesearchtypes", "isstandalonespecial", "collapseboxsetitems", "excludelocationtypes",
			"userid", "api_key", "x-mediabrowser-token", "reqformat", "listitemids", "wassearched", "minpremieredate", "isunaired", "includenextup", "isspecialepisode", "ismissing", "isvirtualunaired", "collectiontypes", "canedititems":
		default:
			if !strings.HasPrefix(key, "x-emby-") {
				fail(w, 400, "UnsupportedQuery")
				return
			}
		}
	}
	for _, key := range []string{"recursive", "isfolder", "isplayed", "isfavorite", "groupprogramsbyseries", "includesearchtypes", "isstandalonespecial", "collapseboxsetitems", "wassearched", "isunaired", "includenextup", "isspecialepisode", "ismissing", "isvirtualunaired", "canedititems"} {
		query[key] = strings.ToLower(query[key])
		if v := query[key]; v != "" && v != "true" && v != "false" {
			fail(w, 400, "InvalidQuery")
			return
		}
	}
	for _, filter := range strings.Split(query["filters"], ",") {
		if filter != "" && !member("IsUnplayed,IsPlayed,IsFavorite,IsFolder,IsNotFolder", filter) {
			fail(w, 400, "UnsupportedFilter")
			return
		}
	}
	// Every catalogued item is a file on disk, so excluding the other location types is a no-op.
	for _, location := range strings.Split(query["excludelocationtypes"], ",") {
		if location != "" && !member("Virtual,Offline,Remote", location) {
			fail(w, 400, "UnsupportedQuery")
			return
		}
	}
	start, limit := 0, 100
	if latest {
		limit = 20
	}
	if resume {
		limit = 24
		query["recursive"] = "true"
	}
	if typesOnly {
		query["recursive"] = "true"
	}
	for key, target := range map[string]*int{"startindex": &start, "limit": &limit} {
		if text, exists := query[key]; exists {
			v, err := strconv.Atoi(text)
			if err != nil || v < 0 || (key == "limit" && v > 1000) {
				fail(w, 400, "InvalidPagination")
				return
			}
			*target = v
		}
	}
	if text, exists := query["minpremieredate"]; exists {
		if _, err := time.Parse(time.RFC3339, text); err != nil {
			fail(w, 400, "InvalidQuery")
			return
		}
	}
	// Catalogued items carry no premiere date, so they never satisfy a premiere
	// bound and are never unaired; Emby likewise omits undated items. The result
	// is empty whatever the requested sort, so the sort is not validated.
	if query["minpremieredate"] != "" || query["isunaired"] == "true" {
		if latest {
			respond(w, 200, []any{})
		} else {
			respond(w, 200, object{"Items": []any{}, "TotalRecordCount": 0})
		}
		return
	}
	sortBy, order := strings.ToLower(query["sortby"]), strings.ToLower(query["sortorder"])
	if latest && sortBy == "" {
		sortBy, order = "datecreated", "descending"
	}
	if resume && sortBy == "" {
		sortBy, order = "dateplayed", "descending"
	}
	if sortBy == "" {
		sortBy = "sortname"
	}
	sortKeys := strings.Split(sortBy, ",")
	for _, key := range sortKeys {
		if !member("sortname,name,datecreated,dateplayed,runtime,indexnumber,parentindexnumber,datelastsearched,isfolder,filename,seriessortname,channelnumber", key) {
			fail(w, 400, "UnsupportedSort")
			return
		}
	}
	if order != "" && order != "ascending" && order != "descending" {
		fail(w, 400, "UnsupportedSort")
		return
	}
	snapshot := s.store.Snapshot()
	serverID := snapshot.ServerID
	// Continue Watching also offers each started series' next episode, dated
	// by when the series was last watched. The reference Emby includes them
	// without IncludeNextUp; only IncludeNextUp=false leaves them out.
	nextUp := map[string]time.Time{}
	if resume && query["includenextup"] != "false" {
		for _, c := range s.nextUpEpisodes(snapshot.User.Items, "", "") {
			if !snapshot.User.Items[c.next.ID].HiddenFromResume {
				nextUp[c.next.ID] = c.lastPlayed
			}
		}
	}
	lastPlayed := func(id string) time.Time {
		played := snapshot.User.Items[id].LastPlayed
		if series, ok := nextUp[id]; ok && series.After(played) {
			return series
		}
		return played
	}
	// Select and paginate metadata before allocating response DTOs.
	items := []*media.Item{}
	isCollection := func(item *media.Item) bool {
		return catalog != nil && (item.ID == catalog.ID || item.ID == catalog.SeriesLibraryID())
	}
	// collectionType names the library an item belongs to, for CollectionTypes;
	// Coach has only movie and TV libraries (#76).
	collectionType := func(item *media.Item) string {
		if (catalog != nil && item.ID == catalog.ID) || (!isCollection(item) && item.Type() == "Movie") {
			return "movies"
		}
		return "tvshows"
	}
	if catalog != nil {
		parent := query["parentid"]
		root := parent == "" || parent == "root"
		if root && query["recursive"] != "true" && !latest && query["ids"] == "" {
			items = append(items, &media.Item{ID: catalog.ID, Name: "Movies"})
			if len(catalog.Folders) > 0 {
				items = append(items, &media.Item{ID: catalog.SeriesLibraryID(), Name: "TV Shows"})
			}
		} else {
			items = make([]*media.Item, 0, len(catalog.Items)+1)
			for _, candidates := range [][]media.Item{catalog.Items, catalog.Folders} {
				for i := range candidates {
					item := &candidates[i]
					if (latest || resume) && item.IsFolder() {
						continue
					}
					if root || catalog.Parent(*item) == parent || ((query["recursive"] == "true" || latest || resume) && (item.SeriesID == parent || (parent == catalog.SeriesLibraryID() && item.Type() != "Movie"))) {
						items = append(items, item)
					}
				}
			}
			if query["ids"] != "" && root {
				items = append(items, &media.Item{ID: catalog.ID, Name: "Movies"})
				if len(catalog.Folders) > 0 {
					items = append(items, &media.Item{ID: catalog.SeriesLibraryID(), Name: "TV Shows"})
				}
			}
		}
	}
	items = slices.DeleteFunc(items, func(item *media.Item) bool {
		id, kind, folder := item.ID, item.Type(), item.IsFolder() || isCollection(item)
		st := snapshot.User.Items[id]
		if item.IsFolder() {
			st.Played, _ = s.folderPlayed(*item, snapshot.User.Items)
		}
		if isCollection(item) {
			kind = "CollectionFolder"
		}
		return (query["ids"] != "" && !member(query["ids"], id)) || member(query["excludeitemids"], id) ||
			(query["collectiontypes"] != "" && !member(query["collectiontypes"], collectionType(item))) ||
			(resume && !resumable(st, item.RunTimeTicks) && nextUp[id].IsZero()) ||
			(query["includeitemtypes"] != "" && !member(query["includeitemtypes"], kind)) || member(query["excludeitemtypes"], kind) ||
			(query["mediatypes"] != "" && (folder || !member(query["mediatypes"], "Video"))) ||
			(query["isfolder"] != "" && (query["isfolder"] == "true") != folder) ||
			!strings.Contains(strings.ToLower(item.Name), strings.ToLower(query["searchterm"])) ||
			(query["isplayed"] != "" && (query["isplayed"] == "true") != st.Played) ||
			(query["isfavorite"] != "" && (query["isfavorite"] == "true") != st.IsFavorite) ||
			(query["wassearched"] != "" && (query["wassearched"] == "true") == st.LastSearched.IsZero()) ||
			(member(query["filters"], "IsPlayed") && !st.Played) ||
			(member(query["filters"], "IsUnplayed") && st.Played) ||
			(member(query["filters"], "IsFavorite") && !st.IsFavorite) ||
			(member(query["filters"], "IsFolder") && !folder) ||
			(member(query["filters"], "IsNotFolder") && folder) ||
			// Without airing-order metadata, every Season 00 episode is a standalone special.
			(query["isstandalonespecial"] != "" && (query["isstandalonespecial"] == "true") != (kind == "Episode" && item.SeasonNumber == 0)) ||
			// Every catalogued item is a file on disk: none is missing or a
			// virtual unaired episode. Playing an episode sends both as false.
			query["ismissing"] == "true" || query["isvirtualunaired"] == "true" ||
			// The library is read-only and Coach has no playlists or
			// collections, so the user can edit no item (#77).
			query["canedititems"] == "true" ||
			// The series page lists specials (Season 00 episodes) in their own row.
			(query["isspecialepisode"] != "" && (query["isspecialepisode"] == "true") != (kind == "Episode" && item.SeasonNumber == 0))
	})
	if typesOnly {
		// Search tabs describe the full filtered result, independent of its page.
		names := []string{}
		for _, item := range items {
			kind := item.Type()
			if isCollection(item) {
				kind = "CollectionFolder"
			}
			if !slices.Contains(names, kind) {
				names = append(names, kind)
			}
		}
		slices.Sort(names)
		result := make([]object, 0, len(names))
		for _, name := range names {
			result = append(result, object{"Name": name})
		}
		respond(w, 200, object{"Items": result, "TotalRecordCount": len(result)})
		return
	}
	slices.SortFunc(items, func(a, b *media.Item) int {
		comparison := 0
		// Keys are compared in order; SortOrder applies to all of them.
		for _, key := range sortKeys {
			switch key {
			case "datecreated":
				comparison = a.Added.Compare(b.Added)
			case "dateplayed":
				comparison = lastPlayed(a.ID).Compare(lastPlayed(b.ID))
			case "datelastsearched":
				comparison = snapshot.User.Items[a.ID].LastSearched.Compare(snapshot.User.Items[b.ID].LastSearched)
			case "runtime":
				comparison = cmp.Compare(a.RunTimeTicks, b.RunTimeTicks)
			case "parentindexnumber":
				comparison = cmp.Compare(a.SeasonNumber, b.SeasonNumber)
			case "indexnumber":
				comparison = cmp.Compare(a.EpisodeNumber, b.EpisodeNumber)
			case "isfolder":
				// Folders first, as in a file browser.
				comparison = -cmp.Compare(boolRank(a.IsFolder() || isCollection(a)), boolRank(b.IsFolder() || isCollection(b)))
			case "filename":
				comparison = strings.Compare(strings.ToLower(path.Base(a.Path)), strings.ToLower(path.Base(b.Path)))
			case "seriessortname":
				comparison = strings.Compare(strings.ToLower(a.SeriesName), strings.ToLower(b.SeriesName))
			case "channelnumber":
				// Coach has no channels: no item has a channel number (#76).
				comparison = 0
			default:
				comparison = strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
			}
			if comparison != 0 {
				break
			}
		}
		if comparison == 0 {
			comparison = strings.Compare(a.ID, b.ID)
		}
		if order == "descending" {
			return -comparison
		}
		return comparison
	})
	total := len(items)
	start = min(start, total)
	items = items[start : start+min(limit, total-start)]
	result := make([]object, 0, len(items))
	for _, item := range items {
		if isCollection(item) {
			result = append(result, s.collectionDTO(item.ID, serverID, false))
		} else {
			result = append(result, s.movieDTO(*item, serverID, snapshot.User.Items, token))
		}
	}
	if latest {
		respond(w, 200, result)
	} else {
		respond(w, 200, object{"Items": result, "TotalRecordCount": total})
	}
}
