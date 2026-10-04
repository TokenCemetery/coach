package server

import (
	"testing"
	"testing/fstest"
)

// TestLocaleFiles keeps every locale aligned with the English fallback: no
// unknown keys (a typo would silently show English) and no empty texts.
func TestLocaleFiles(t *testing.T) {
	for language, texts := range locales {
		for key, text := range texts {
			if _, known := locales[fallbackLanguage][key]; !known {
				t.Errorf("%s.yaml: key %s is not in %s.yaml", language, key, fallbackLanguage)
			}
			if text == "" {
				t.Errorf("%s.yaml: key %s is empty", language, key)
			}
		}
	}
	for _, language := range []string{"ru", "de", "fr", "es", "it"} {
		if len(locales[language]) != len(locales[fallbackLanguage]) {
			t.Errorf("%s.yaml translates %d of %d keys", language, len(locales[language]), len(locales[fallbackLanguage]))
		}
	}
}

func TestLoadLocalesRejectsBrokenFiles(t *testing.T) {
	for name, fsys := range map[string]fstest.MapFS{
		"malformed":   {"locales/en.yaml": {Data: []byte("HeaderMyMedia: [unclosed")}},
		"no fallback": {"locales/ru.yaml": {Data: []byte("HeaderMyMedia: Мои медиаданные")}},
		"nested":      {"locales/en.yaml": {Data: []byte("Header:\n  MyMedia: My Media")}},
	} {
		if _, err := loadLocales(fsys); err == nil {
			t.Errorf("%s: loaded without error", name)
		}
	}
}

func TestTranslateFallback(t *testing.T) {
	for _, tc := range []struct{ language, key, want string }{
		{"ru", "HeaderMyMedia", "Мои медиаданные"},
		{"fr_CA", "HeaderMyMedia", "Mes Médias"},
		{"ja", "HeaderMyMedia", "My Media"},
		{"ru", "UnknownKey", "UnknownKey"},
	} {
		if got := translate(tc.language, tc.key); got != tc.want {
			t.Errorf("translate(%q, %q) = %q, want %q", tc.language, tc.key, got, tc.want)
		}
	}
}
