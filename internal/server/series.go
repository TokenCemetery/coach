package server

import (
	"cmp"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/TokenCemetery/coach/internal/media"
	"github.com/TokenCemetery/coach/internal/state"
)

func (s *Server) seriesRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /shows/nextup", s.protect(s.nextUp))
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

// nextUp lists, per series the user has started, the first unplayed episode
// after the furthest played one, most recently watched series first. Specials
// (Season 00) are skipped because Coach has no airing-order metadata for them.
func (s *Server) nextUp(w http.ResponseWriter, r *http.Request, token string, session state.Session) {
	query, err := catalogQuery(r)
	if err != nil {
		fail(w, 400, "InvalidQuery")
		return
	}
	for key := range query {
		if !member("userid,seriesid,parentid,startindex,limit,fields,enableimages,enableimagetypes,imagetypelimit,enableuserdata,enabletotalrecordcount,legacynextup,api_key,x-mediabrowser-token,reqformat", key) && !strings.HasPrefix(key, "x-emby-") {
			fail(w, 400, "UnsupportedQuery")
			return
		}
	}
	start, limit := 0, 100
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
	snapshot := s.store.Snapshot()
	candidates := s.nextUpEpisodes(snapshot.User.Items, query["seriesid"], query["parentid"])
	total := len(candidates)
	start = min(start, total)
	result := make([]object, 0, min(limit, total-start))
	for _, c := range candidates[start : start+min(limit, total-start)] {
		result = append(result, s.movieDTO(*c.next, snapshot.ServerID, snapshot.User.Items, token))
	}
	respond(w, 200, object{"Items": result, "TotalRecordCount": total})
}

// nextUpCandidate is a started series' next episode and when the series was
// last watched.
type nextUpCandidate struct {
	next       *media.Item
	lastPlayed time.Time
}

// nextUpEpisodes finds, per started series, the first unplayed episode after
// the furthest played one, most recently watched series first. seriesID and
// parent narrow the series; empty values select all.
func (s *Server) nextUpEpisodes(states map[string]state.ItemState, seriesID, parent string) []nextUpCandidate {
	candidates := []nextUpCandidate{}
	if s.media == nil {
		return candidates
	}
	episodes := map[string][]*media.Item{}
	for i := range s.media.Items {
		item := &s.media.Items[i]
		if item.Type() != "Episode" || item.SeasonNumber == 0 ||
			(seriesID != "" && item.SeriesID != seriesID) ||
			(parent != "" && parent != rootID && parent != s.media.SeriesLibraryID() && parent != item.SeriesID) {
			continue
		}
		episodes[item.SeriesID] = append(episodes[item.SeriesID], item)
	}
	for _, list := range episodes {
		slices.SortFunc(list, func(a, b *media.Item) int {
			return cmp.Or(cmp.Compare(a.SeasonNumber, b.SeasonNumber), cmp.Compare(a.EpisodeNumber, b.EpisodeNumber), strings.Compare(a.ID, b.ID))
		})
		furthest, c := -1, nextUpCandidate{}
		for i, item := range list {
			st := states[item.ID]
			if st.Played {
				furthest = i
			}
			if st.LastPlayed.After(c.lastPlayed) {
				c.lastPlayed = st.LastPlayed
			}
		}
		// Nothing after the furthest played episode is played, so it is the next one.
		if furthest >= 0 && furthest+1 < len(list) {
			c.next = list[furthest+1]
			candidates = append(candidates, c)
		}
	}
	slices.SortFunc(candidates, func(a, b nextUpCandidate) int {
		return cmp.Or(b.lastPlayed.Compare(a.lastPlayed), strings.Compare(a.next.SeriesID, b.next.SeriesID))
	})
	return candidates
}
