package server

import (
	_ "embed"
	"encoding/json"
	"strings"

	"github.com/TokenCemetery/coach/internal/media"
)

// culturesJSON is Emby 4.10.0.40's GET /Localization/Cultures response,
// captured read-only from the reference server: public ISO 639 data. Emby Web
// lists these languages on the subtitle and playback settings pages and saves
// the chosen TwoLetterISOLanguageName values.
//
//go:embed cultures.json
var culturesJSON []byte

type culture struct {
	TwoLetterISOLanguageName    string
	ThreeLetterISOLanguageNames []string
}

// languageCodes maps every two- and three-letter code of a language to its
// two-letter code, so "rus", "ru" and "RU" compare equal.
var languageCodes = mustLanguageCodes(culturesJSON)

func mustLanguageCodes(data []byte) map[string]string {
	var list []culture
	if err := json.Unmarshal(data, &list); err != nil || len(list) == 0 {
		panic("cultures.json is broken")
	}
	codes := map[string]string{}
	for _, c := range list {
		codes[c.TwoLetterISOLanguageName] = c.TwoLetterISOLanguageName
		for _, three := range c.ThreeLetterISOLanguageNames {
			codes[three] = c.TwoLetterISOLanguageName
		}
	}
	return codes
}

// normalLanguage returns the two-letter code of a language code, or the code
// itself in lower case when the list does not know it.
func normalLanguage(code string) string {
	code = strings.ToLower(strings.TrimSpace(code))
	if two, ok := languageCodes[code]; ok {
		return two
	}
	return code
}

// subtitleSelection is the subtitle part of the user configuration.
type subtitleSelection struct {
	SubtitleMode               string
	SubtitleLanguagePreference string
}

// defaultSubtitle picks the subtitle to show when the client leaves the choice
// to the server, following Emby Web's own descriptions of each mode:
//
//   - Default: tracks flagged default or forced; language preference breaks ties.
//   - Smart: a preferred-language track when the audio is in another language.
//   - OnlyForced: forced tracks only.
//   - HearingImpaired: preferred-language tracks, SDH ones first.
//   - Always: preferred-language tracks regardless of the audio language.
//   - None: no subtitle.
//
// Only tracks deliverable reports true for are considered. -1 means none.
func defaultSubtitle(item media.Item, config subtitleSelection, audio *media.Stream, deliverable func(int) bool) int {
	preferred := []string{}
	for _, code := range strings.Split(config.SubtitleLanguagePreference, ",") {
		if code = normalLanguage(code); code != "" {
			preferred = append(preferred, code)
		}
	}
	rank := func(s media.Stream) int {
		for i, code := range preferred {
			if normalLanguage(s.Language) == code {
				return i
			}
		}
		return len(preferred)
	}
	matchesPreference := func(s media.Stream) bool { return len(preferred) == 0 || rank(s) < len(preferred) }
	var accept func(media.Stream) bool
	sdhFirst := false
	switch config.SubtitleMode {
	case "None":
		return -1
	case "OnlyForced":
		accept = func(s media.Stream) bool { return s.IsForced }
	case "Smart":
		if len(preferred) == 0 || audio == nil || rank(media.Stream{Language: audio.Language}) < len(preferred) {
			return -1
		}
		accept = func(s media.Stream) bool { return !s.IsForced && rank(s) < len(preferred) }
	case "Always":
		accept = func(s media.Stream) bool { return !s.IsForced && matchesPreference(s) }
	case "HearingImpaired":
		accept = func(s media.Stream) bool { return !s.IsForced && matchesPreference(s) }
		sdhFirst = true
	default:
		accept = func(s media.Stream) bool { return s.IsDefault || s.IsForced }
	}
	best := -1
	var bestStream media.Stream
	better := func(s media.Stream) bool {
		if best < 0 || rank(s) != rank(bestStream) {
			return best < 0 || rank(s) < rank(bestStream)
		}
		if sdhFirst && s.IsHearingImpaired != bestStream.IsHearingImpaired {
			return s.IsHearingImpaired
		}
		// Forced tracks carry dialogue in a foreign language, so they come
		// before tracks that are merely flagged default.
		if s.IsForced != bestStream.IsForced {
			return s.IsForced
		}
		return s.IsDefault && !bestStream.IsDefault
	}
	for _, s := range item.Streams {
		if s.Type == "Subtitle" && accept(s) && deliverable(s.Index) && better(s) {
			best, bestStream = s.Index, s
		}
	}
	return best
}
