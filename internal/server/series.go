package server

import (
	"net/http"
	"strings"

	"github.com/TokenCemetery/coach/internal/state"
)

func (s *Server) seriesRoutes(mux *http.ServeMux) {
	for _, kind := range []string{"Season", "Episode"} {
		mux.HandleFunc("GET /shows/{item}/"+strings.ToLower(kind)+"s", s.protect(func(w http.ResponseWriter, r *http.Request, token string, session state.Session) {
			series, found := s.findItem(r.PathValue("item"))
			if !found || series.Type() != "Series" {
				fail(w, 404, "NotFound")
				return
			}
			query, err := catalogQuery(r)
			if err != nil {
				fail(w, 400, "InvalidQuery")
				return
			}
			parent := series.ID
			if id := query["seasonid"]; id != "" {
				season, found := s.findItem(id)
				if kind != "Episode" || !found || season.Type() != "Season" || season.SeriesID != series.ID {
					fail(w, 404, "NotFound")
					return
				}
				parent = id
			}
			q := r.URL.Query()
			excluded := query["excludeitemids"]
			if special, exists := query["isspecialseason"]; exists && kind == "Season" {
				special = strings.ToLower(special)
				if special != "true" && special != "false" {
					fail(w, 400, "InvalidQuery")
					return
				}
				// Season 00 holds specials; exclude seasons on the other side of the filter.
				for _, folder := range s.media.Folders {
					if folder.Type() == "Season" && folder.SeriesID == series.ID && (folder.SeasonNumber == 0) != (special == "true") {
						excluded += "," + folder.ID
					}
				}
			}
			for key := range q {
				if member("parentid,recursive,includeitemtypes,seasonid,excludeitemids", key) || (kind == "Season" && strings.EqualFold(key, "isspecialseason")) {
					q.Del(key)
				}
			}
			if excluded = strings.Trim(excluded, ","); excluded != "" {
				q.Set("ExcludeItemIds", excluded)
			}
			q.Set("ParentId", parent)
			q.Set("Recursive", "true")
			q.Set("IncludeItemTypes", kind)
			if query["sortby"] == "" {
				q.Set("SortBy", "ParentIndexNumber,IndexNumber")
			}
			r.URL.RawQuery = q.Encode()
			s.listItems(w, r, false, false)
		}))
	}
}
