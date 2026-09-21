package media

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestScanSeriesHierarchy(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"Show/Season 01/Show.S01E02.mp4", "Show/Season 01/Show.S01E01.mp4", "Show/Season 00/Show.S00E01.mp4", "Other/Season 01/Other.S01E01.mp4", "Movie.S01E01.mp4", "Show/Season 01/Wrong.S02E01.mp4"} {
		file := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("video"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	probe := func(context.Context, *os.File) (Item, error) { return Item{RunTimeTicks: 100000000}, nil }
	catalog, err := scan(context.Background(), dir, probe)
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	if len(catalog.Items) != 6 || len(catalog.Folders) != 5 {
		t.Fatalf("items=%d folders=%d", len(catalog.Items), len(catalog.Folders))
	}
	episodes := 0
	for _, item := range catalog.Items {
		if item.Type() != "Episode" {
			continue
		}
		episodes++
		if item.SeriesID == "" || item.ParentID != item.SeasonID || item.EpisodeNumber < 1 {
			t.Fatal("incomplete hierarchy", item)
		}
		file, err := catalog.Open(item)
		if err != nil {
			t.Fatal(err)
		}
		file.Close()
	}
	if episodes != 4 {
		t.Fatalf("episodes=%d", episodes)
	}
	again, err := scan(context.Background(), dir, probe)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if !reflect.DeepEqual(catalog.Items, again.Items) || !reflect.DeepEqual(catalog.Folders, again.Folders) {
		t.Fatal("hierarchy changed on rescan")
	}
}
