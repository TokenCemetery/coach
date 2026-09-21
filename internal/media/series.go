package media

import (
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Only an explicit Show/Season NN/name.SnnEnn.ext layout is classified as TV.
// Ambiguous names remain movies; no external metadata or filename guessing.
var seasonDirectory = regexp.MustCompile(`(?i)^season[ ._-]*(\d{1,3})$`)
var episodeName = regexp.MustCompile(`(?i)(?:^|[ ._-])s(\d{1,3})e(\d{1,4})(?:$|[ ._-])`)

func (c *Catalog) SeriesLibraryID() string { return stableID("series\x00" + c.ID) }

func (i Item) Type() string {
	if i.Kind == "" {
		return "Movie"
	}
	return i.Kind
}

func (i Item) IsFolder() bool { return i.Kind == "Series" || i.Kind == "Season" }

func (c *Catalog) Parent(item Item) string {
	if item.ParentID != "" {
		return item.ParentID
	}
	return c.ID
}

func (c *Catalog) groupEpisodes() {
	folders := map[string]Item{}
	for n := range c.Items {
		item := &c.Items[n]
		seasonPath := path.Dir(item.Path)
		seriesPath := path.Dir(seasonPath)
		seasonMatch := seasonDirectory.FindStringSubmatch(path.Base(seasonPath))
		episodeMatch := episodeName.FindStringSubmatch(item.Name)
		if seriesPath == "." || len(seasonMatch) == 0 || len(episodeMatch) == 0 {
			continue
		}
		season, _ := strconv.Atoi(seasonMatch[1])
		episodeSeason, _ := strconv.Atoi(episodeMatch[1])
		if season != episodeSeason {
			continue
		}
		episode, _ := strconv.Atoi(episodeMatch[2])
		seriesID := stableID(c.SeriesLibraryID() + "\x00" + seriesPath)
		seasonID := stableID(seriesID + "\x00" + seasonPath)
		seriesName := path.Base(seriesPath)
		item.Kind, item.ParentID = "Episode", seasonID
		item.SeriesID, item.SeriesName, item.SeasonID = seriesID, seriesName, seasonID
		item.SeasonNumber, item.EpisodeNumber = season, episode
		for _, folder := range []Item{
			{ID: seriesID, Name: seriesName, Kind: "Series", Path: seriesPath, ParentID: c.SeriesLibraryID()},
			{ID: seasonID, Name: "Season " + strconv.Itoa(season), Kind: "Season", Path: seasonPath, ParentID: seriesID, SeriesID: seriesID, SeriesName: seriesName, SeasonNumber: season},
		} {
			folder.Modified = item.Modified
			if old, exists := folders[folder.ID]; exists && old.Modified.After(folder.Modified) {
				folder.Modified = old.Modified
			}
			folders[folder.ID] = folder
		}
	}
	c.Folders = make([]Item, 0, len(folders))
	for _, folder := range folders {
		c.Folders = append(c.Folders, folder)
	}
	slices.SortFunc(c.Folders, func(a, b Item) int { return strings.Compare(a.ID, b.ID) })
}
