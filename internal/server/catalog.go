package server

import (
	"cmp"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

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
			if s.media != nil {
				snapshot := s.store.Snapshot()
				if id == s.media.ID || (len(s.media.Folders) > 0 && id == s.media.SeriesLibraryID()) {
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
	s.extrasRoutes(mux)
	s.seriesRoutes(mux)
	s.imageRoutes(mux)
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
}

func (s *Server) listItems(w http.ResponseWriter, r *http.Request, latest, resume bool) {
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
			"groupprogramsbyseries", "includesearchtypes",
			"userid", "api_key", "x-mediabrowser-token", "reqformat", "listitemids":
		default:
			if !strings.HasPrefix(key, "x-emby-") {
				fail(w, 400, "UnsupportedQuery")
				return
			}
		}
	}
	for _, key := range []string{"recursive", "isfolder", "isplayed", "isfavorite", "groupprogramsbyseries", "includesearchtypes"} {
		query[key] = strings.ToLower(query[key])
		if v := query[key]; v != "" && v != "true" && v != "false" {
			fail(w, 400, "InvalidQuery")
			return
		}
	}
	for _, filter := range strings.Split(query["filters"], ",") {
		if filter != "" && !member("IsUnplayed,IsPlayed,IsFavorite", filter) {
			fail(w, 400, "UnsupportedFilter")
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
	if (!member("sortname,name,datecreated,dateplayed,runtime,indexnumber", sortBy) && sortBy != "parentindexnumber,indexnumber") || (order != "" && order != "ascending" && order != "descending") {
		fail(w, 400, "UnsupportedSort")
		return
	}
	snapshot := s.store.Snapshot()
	serverID := snapshot.ServerID
	// Select and paginate metadata before allocating response DTOs.
	items := []*media.Item{}
	isCollection := func(item *media.Item) bool {
		return s.media != nil && (item.ID == s.media.ID || item.ID == s.media.SeriesLibraryID())
	}
	if s.media != nil {
		parent := query["parentid"]
		root := parent == "" || parent == "root"
		if root && query["recursive"] != "true" && !latest && query["ids"] == "" {
			items = append(items, &media.Item{ID: s.media.ID, Name: "Movies"})
			if len(s.media.Folders) > 0 {
				items = append(items, &media.Item{ID: s.media.SeriesLibraryID(), Name: "TV Shows"})
			}
		} else {
			items = make([]*media.Item, 0, len(s.media.Items)+1)
			for _, candidates := range [][]media.Item{s.media.Items, s.media.Folders} {
				for i := range candidates {
					item := &candidates[i]
					if (latest || resume) && item.IsFolder() {
						continue
					}
					if root || s.media.Parent(*item) == parent || ((query["recursive"] == "true" || latest || resume) && (item.SeriesID == parent || (parent == s.media.SeriesLibraryID() && item.Type() != "Movie"))) {
						items = append(items, item)
					}
				}
			}
			if query["ids"] != "" && root {
				items = append(items, &media.Item{ID: s.media.ID, Name: "Movies"})
				if len(s.media.Folders) > 0 {
					items = append(items, &media.Item{ID: s.media.SeriesLibraryID(), Name: "TV Shows"})
				}
			}
		}
	}
	items = slices.DeleteFunc(items, func(item *media.Item) bool {
		id, kind, folder := item.ID, item.Type(), item.IsFolder() || isCollection(item)
		st := snapshot.User.Items[id]
		if isCollection(item) {
			kind = "CollectionFolder"
		}
		return (query["ids"] != "" && !member(query["ids"], id)) || member(query["excludeitemids"], id) ||
			(resume && !resumable(st, item.RunTimeTicks)) ||
			(query["includeitemtypes"] != "" && !member(query["includeitemtypes"], kind)) || member(query["excludeitemtypes"], kind) ||
			(query["mediatypes"] != "" && (folder || !member(query["mediatypes"], "Video"))) ||
			(query["isfolder"] != "" && (query["isfolder"] == "true") != folder) ||
			!strings.Contains(strings.ToLower(item.Name), strings.ToLower(query["searchterm"])) ||
			(query["isplayed"] != "" && (query["isplayed"] == "true") != st.Played) ||
			(query["isfavorite"] != "" && (query["isfavorite"] == "true") != st.IsFavorite) ||
			(member(query["filters"], "IsPlayed") && !st.Played) ||
			(member(query["filters"], "IsUnplayed") && st.Played) ||
			(member(query["filters"], "IsFavorite") && !st.IsFavorite)
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
		switch sortBy {
		case "datecreated":
			comparison = a.Modified.Compare(b.Modified)
		case "dateplayed":
			comparison = snapshot.User.Items[a.ID].LastPlayed.Compare(snapshot.User.Items[b.ID].LastPlayed)
		case "runtime":
			comparison = cmp.Compare(a.RunTimeTicks, b.RunTimeTicks)
		case "indexnumber", "parentindexnumber,indexnumber":
			comparison = cmp.Compare(a.SeasonNumber, b.SeasonNumber)
			if comparison == 0 {
				comparison = cmp.Compare(a.EpisodeNumber, b.EpisodeNumber)
			}
		default:
			comparison = strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
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
