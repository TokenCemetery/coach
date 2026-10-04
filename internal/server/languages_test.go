package server

import (
	"testing"

	"github.com/TokenCemetery/coach/internal/media"
)

func TestDefaultSubtitle(t *testing.T) {
	item := media.Item{Streams: []media.Stream{
		{Index: 0, Type: "Video"},
		{Index: 1, Type: "Audio", Language: "eng"},
		{Index: 2, Type: "Subtitle", Language: "eng", IsForced: true},
		{Index: 3, Type: "Subtitle", Language: "rus"},
		{Index: 4, Type: "Subtitle", Language: "ru", IsHearingImpaired: true},
		{Index: 5, Type: "Subtitle", Language: "ger", IsDefault: true},
		{Index: 6, Type: "Subtitle", Language: "fre", Codec: "hdmv_pgs_subtitle"},
	}}
	english := &item.Streams[1]
	russianAudio := &media.Stream{Type: "Audio", Language: "rus"}
	deliverable := func(index int) bool { return index != 6 }
	for _, tc := range []struct {
		mode, languages string
		audio           *media.Stream
		want            int
	}{
		{"", "", english, 2},                  // Default: forced before default-flagged
		{"Default", "en", english, 2},         // language preference ranks flagged tracks
		{"Default", "ru", english, 2},         // no flagged Russian track: flags still decide
		{"Smart", "ru", english, 3},           // foreign audio: preferred, not forced, first by index
		{"Smart", "ru", russianAudio, -1},     // audio already in the preferred language
		{"Smart", "", english, -1},            // no preference, nothing is foreign
		{"OnlyForced", "ru", english, 2},      // forced only, whatever the language
		{"Always", "de,ru", russianAudio, 5},  // order of preference, not of streams
		{"Always", "ru", english, 3},          // forced tracks are not "always" subtitles
		{"HearingImpaired", "ru", english, 4}, // SDH first within the language
		{"Always", "fr", english, -1},         // the only French track cannot be delivered
		{"None", "ru", english, -1},
	} {
		got := defaultSubtitle(item, subtitleSelection{SubtitleMode: tc.mode, SubtitleLanguagePreference: tc.languages}, tc.audio, deliverable)
		if got != tc.want {
			t.Errorf("mode %q languages %q audio %s: got %d, want %d", tc.mode, tc.languages, tc.audio.Language, got, tc.want)
		}
	}
}
