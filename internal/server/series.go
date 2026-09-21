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
			for key := range q {
				if member("parentid,recursive,includeitemtypes,seasonid", key) {
					q.Del(key)
				}
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
