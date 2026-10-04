package server

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

// fallbackLanguage holds every key; other locales may translate a subset.
const fallbackLanguage = "en"

//go:embed locales/*.yaml
var localeFiles embed.FS

// locales maps a base language ("ru") to its translations by key. A broken
// locale file is a build defect, so loading it fails at startup.
var locales = mustLoadLocales(localeFiles)

func mustLoadLocales(fsys fs.FS) map[string]map[string]string {
	loaded, err := loadLocales(fsys)
	if err != nil {
		panic(err)
	}
	return loaded
}

// loadLocales reads locales/<language>.yaml files of flat key: value strings.
func loadLocales(fsys fs.FS) (map[string]map[string]string, error) {
	names, err := fs.Glob(fsys, "locales/*.yaml")
	if err != nil {
		return nil, err
	}
	loaded := map[string]map[string]string{}
	for _, name := range names {
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, err
		}
		var texts map[string]string
		if err := yaml.Unmarshal(data, &texts); err != nil {
			return nil, fmt.Errorf("locale %s: %w", name, err)
		}
		loaded[strings.TrimSuffix(path.Base(name), ".yaml")] = texts
	}
	if len(loaded[fallbackLanguage]) == 0 {
		return nil, fmt.Errorf("locale %s.yaml is missing or empty", fallbackLanguage)
	}
	return loaded, nil
}

// translate returns the text for key in an X-Emby-Language value such as
// "ru", "es-MX" or "fr_CA", falling back to English, then to the key itself.
func translate(language, key string) string {
	base := strings.ToLower(language)
	if i := strings.IndexAny(base, "-_"); i >= 0 {
		base = base[:i]
	}
	if text := locales[base][key]; text != "" {
		return text
	}
	if text := locales[fallbackLanguage][key]; text != "" {
		return text
	}
	return key
}
