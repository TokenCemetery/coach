package server

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/TokenCemetery/coach/internal/media"
)

func TestLibraryFilters(t *testing.T) {
	store, _, _ := newTestServer(t)
	catalog := &media.Catalog{ID: "movies", Items: []media.Item{
		{ID: "mp4", Name: "Alpha", Container: "MP4", Streams: []media.Stream{
			{Type: "Video", Codec: "h264"}, {Type: "Audio", Codec: "aac", ChannelLayout: "stereo", Language: "eng"},
			{Type: "Subtitle", Codec: "subrip", Language: "rus"}}},
		{ID: "mkv", Name: "Beta", Container: "mkv", Streams: []media.Stream{
			{Type: "Video", Codec: "h264"}, {Type: "Audio", Codec: "ac3", ChannelLayout: "5.1(side)", Language: "rus"}}},
		{ID: "episode", Kind: "Episode", Name: "Show S01E01", Container: "mkv", ParentID: "season", SeriesID: "series", SeasonID: "season", SeasonNumber: 1, EpisodeNumber: 1,
			Streams: []media.Stream{{Type: "Video", Codec: "hevc"}, {Type: "Audio", Codec: "eac3", Language: "jpn"}}},
	}}
	catalog.Folders = []media.Item{
		{ID: "series", Kind: "Series", Name: "Show", ParentID: catalog.SeriesLibraryID()},
		{ID: "season", Kind: "Season", Name: "Season 1", ParentID: "series", SeriesID: "series", SeasonNumber: 1},
	}
	h := New(store, "test", nil, catalog).Handler()
	token := login(t, h)
	user := store.Snapshot().User.ID
	names := func(path string) string {
		t.Helper()
		w := request(h, "GET", path, "", "", token)
		expectStatus(t, w, 200)
		var page struct {
			Items            []struct{ Name, Id string }
			TotalRecordCount int
		}
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		list := []string{}
		for _, item := range page.Items {
			if item.Name != item.Id {
				t.Fatalf("%s: Name %q, Id %q", path, item.Name, item.Id)
			}
			list = append(list, item.Name)
		}
		if page.TotalRecordCount != len(list) {
			t.Fatalf("%s: total %d for %d items", path, page.TotalRecordCount, len(list))
		}
		return strings.Join(list, ",")
	}
	movies, tv := "ParentId=movies&IncludeItemTypes=Movie", "ParentId="+catalog.SeriesLibraryID()+"&IncludeItemTypes=Series"
	for path, want := range map[string]string{
		"/Containers?" + movies:                          "mkv,mp4",
		"/VideoCodecs?" + movies:                         "h264",
		"/VideoCodecs?" + tv:                             "hevc",
		"/VideoCodecs":                                   "h264,hevc",
		"/AudioCodecs?ParentId=series":                   "eac3",
		"/AudioLayouts?" + movies:                        "5.1(side),stereo",
		"/SubtitleCodecs?" + movies:                      "subrip",
		"/StreamLanguages?StreamType=Audio&" + movies:    "eng,rus",
		"/StreamLanguages?StreamType=Subtitle&" + movies: "rus",
		"/Genres?" + movies:                              "",
		"/Years?" + movies:                               "",
		"/Studios":                                       "",
		"/Tags":                                          "",
		"/OfficialRatings":                               "",
		"/ExtendedVideoTypes":                            "",
	} {
		if got := names(path); got != want {
			t.Fatalf("%s: %q, want %q", path, got, want)
		}
	}
	ids := func(query string) string {
		t.Helper()
		w := request(h, "GET", "/Users/"+user+"/Items?Recursive=true&"+query, "", "", token)
		expectStatus(t, w, 200)
		var page struct{ Items []struct{ Id string } }
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		list := []string{}
		for _, item := range page.Items {
			list = append(list, item.Id)
		}
		slices.Sort(list)
		return strings.Join(list, ",")
	}
	for query, want := range map[string]string{
		"Containers=mp4":                                 "mp4",
		"VideoCodecs=H264":                               "mkv,mp4",
		"AudioCodecs=ac3,eac3":                           "episode,mkv,season,series",
		"AudioLayouts=stereo":                            "mp4",
		"SubtitleCodecs=subrip":                          "mp4",
		"AudioLanguages=rus&IncludeItemTypes=Movie":      "mkv",
		"SubtitleLanguages=rus":                          "mp4",
		"VideoCodecs=hevc&IncludeItemTypes=Series":       "series",
		"ExtendedVideoTypes=None&IncludeItemTypes=Movie": "mkv,mp4",
		"ExtendedVideoTypes=Dolby":                       "",
		"Genres=Drama":                                   "",
		"Years=2020":                                     "",
	} {
		if got := ids(query); got != want {
			t.Fatalf("Items?%s: %q, want %q", query, got, want)
		}
	}
}
