package server

import (
	"net/http"
	"slices"
	"strings"

	"github.com/TokenCemetery/coach/internal/media"
	"github.com/TokenCemetery/coach/internal/state"
)

// mediaFilters maps the Items filters on stream metadata to the values an
// item offers for them; Emby Web's library filter dialog lists the values
// and sends the chosen ones (#78).
var mediaFilters = map[string]func(item media.Item) []string{
	"containers":        func(item media.Item) []string { return nonEmpty(strings.ToLower(item.Container)) },
	"videocodecs":       streamValues("Video", func(s media.Stream) string { return s.Codec }),
	"audiocodecs":       streamValues("Audio", func(s media.Stream) string { return s.Codec }),
	"audiolayouts":      streamValues("Audio", func(s media.Stream) string { return s.ChannelLayout }),
	"subtitlecodecs":    streamValues("Subtitle", func(s media.Stream) string { return s.Codec }),
	"audiolanguages":    streamValues("Audio", func(s media.Stream) string { return s.Language }),
	"subtitlelanguages": streamValues("Subtitle", func(s media.Stream) string { return s.Language }),
	// Every item reports ExtendedVideoType None (dto.go).
	"extendedvideotypes": func(media.Item) []string { return []string{"None"} },
}

// metadataFilters are Items filters on metadata Coach does not have, so a
// value given for them matches no item.
var metadataFilters = []string{"genres", "studios", "tags", "officialratings", "years"}

func nonEmpty(values ...string) []string {
	return slices.DeleteFunc(values, func(v string) bool { return v == "" })
}

func streamValues(kind string, value func(media.Stream) string) func(media.Item) []string {
	return func(item media.Item) []string {
		values := []string{}
		for _, stream := range item.Streams {
			if stream.Type == kind {
				values = append(values, nonEmpty(value(stream))...)
			}
		}
		return values
	}
}

// filterValues returns the values item offers for an Items filter; a series
// or season offers those of its episodes.
func filterValues(catalog *media.Catalog, item *media.Item, values func(media.Item) []string) []string {
	if !item.IsFolder() {
		return values(*item)
	}
	result := []string{}
	for _, episode := range catalog.Items {
		if episode.SeriesID == item.ID || episode.SeasonID == item.ID {
			result = append(result, values(episode)...)
		}
	}
	return result
}

// failsMediaFilters reports whether item is excluded by the stream and
// metadata filters in query, whose keys are lower case.
func failsMediaFilters(catalog *media.Catalog, item *media.Item, query map[string]string) bool {
	for _, key := range metadataFilters {
		if query[key] != "" {
			return true
		}
	}
	for key, values := range mediaFilters {
		wanted := query[key]
		if wanted == "" {
			continue
		}
		if !slices.ContainsFunc(filterValues(catalog, item, values), func(v string) bool { return member(wanted, v) }) {
			return true
		}
	}
	return false
}

// filterValueList answers the value lists of the library filter dialog:
// the distinct values of the videos in scope (ParentId, IncludeItemTypes),
// each as a TagItem with the same Name and Id. values is nil for metadata
// Coach does not have, which lists nothing.
func (s *Server) filterValueList(values func(media.Item) []string) authenticated {
	return func(w http.ResponseWriter, r *http.Request, token string, session state.Session) {
		query := r.URL.Query()
		catalog := s.catalog()
		found := []string{}
		if catalog != nil && values != nil {
			parent, types := query.Get("ParentId"), query.Get("IncludeItemTypes")
			for i := range catalog.Items {
				item := &catalog.Items[i]
				if !inFilterScope(catalog, item, parent, types) {
					continue
				}
				for _, v := range values(*item) {
					if !slices.ContainsFunc(found, func(f string) bool { return strings.EqualFold(f, v) }) {
						found = append(found, v)
					}
				}
			}
		}
		slices.SortFunc(found, func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) })
		items := make([]object, 0, len(found))
		for _, v := range found {
			items = append(items, object{"Name": v, "Id": v})
		}
		respond(w, 200, object{"Items": items, "TotalRecordCount": len(items)})
	}
}

// inFilterScope reports whether a movie or episode belongs to the library,
// series or season parent and to the requested item types, where a series
// or season type stands for its episodes.
func inFilterScope(catalog *media.Catalog, item *media.Item, parent, types string) bool {
	kind := item.Type()
	switch parent {
	case "", "root":
	case catalog.ID:
		if kind != "Movie" {
			return false
		}
	case catalog.SeriesLibraryID():
		if kind == "Movie" {
			return false
		}
	default:
		if item.SeriesID != parent && item.SeasonID != parent && item.ID != parent {
			return false
		}
	}
	if types == "" {
		return true
	}
	return member(types, kind) || (kind == "Episode" && (member(types, "Series") || member(types, "Season")))
}

func (s *Server) filterRoutes(mux *http.ServeMux) {
	for path, key := range map[string]string{
		"/containers": "containers", "/videocodecs": "videocodecs", "/audiocodecs": "audiocodecs",
		"/audiolayouts": "audiolayouts", "/subtitlecodecs": "subtitlecodecs",
	} {
		mux.HandleFunc("GET "+path, s.protect(s.filterValueList(mediaFilters[key])))
	}
	mux.HandleFunc("GET /streamlanguages", s.protect(func(w http.ResponseWriter, r *http.Request, token string, session state.Session) {
		key := "audiolanguages"
		if strings.EqualFold(r.URL.Query().Get("StreamType"), "Subtitle") {
			key = "subtitlelanguages"
		}
		s.filterValueList(mediaFilters[key])(w, r, token, session)
	}))
	// Coach has no genres, studios, tags, ratings or years, and every video
	// is of the plain type, so these lists are empty.
	for _, path := range []string{"/genres", "/studios", "/tags", "/officialratings", "/years", "/extendedvideotypes"} {
		mux.HandleFunc("GET "+path, s.protect(s.filterValueList(nil)))
	}
}
