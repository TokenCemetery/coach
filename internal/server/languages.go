package server

import _ "embed"

// culturesJSON is Emby 4.10.0.40's GET /Localization/Cultures response,
// captured read-only from the reference server: public ISO 639 data. Emby Web
// lists these languages on the subtitle and playback settings pages and saves
// the chosen TwoLetterISOLanguageName values.
//
//go:embed cultures.json
var culturesJSON []byte
