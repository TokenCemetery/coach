// Package media builds a read-only video catalogue from an explicitly selected directory.
package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

const maxEntries = 100000
const maxMovies = 10000

// Catalog and its items are immutable after Scan returns. The scan root stays
// open so playback can reopen a file later without re-resolving its path
// outside the directory the operator selected.
type Catalog struct {
	ID      string
	Items   []Item
	Folders []Item
	Skipped int
	root    *os.Root
}

type Item struct {
	ID            string
	Name          string
	Kind          string
	ParentID      string
	SeriesID      string
	SeriesName    string
	SeasonID      string
	SeasonNumber  int
	EpisodeNumber int
	// Path is relative to the catalogue root and is never sent to clients.
	Path         string
	Container    string
	Size         int64
	Modified     time.Time
	RunTimeTicks int64
	Streams      []Stream
}

// Open reopens an item's file read-only, confined to the catalogue root.
func (c *Catalog) Open(item Item) (*os.File, error) {
	if c == nil || c.root == nil {
		return nil, errors.New("catalogue has no open root")
	}
	file, err := c.root.OpenFile(filepath.FromSlash(item.Path), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("media file is unavailable")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, errors.New("media file is unavailable")
	}
	return file, nil
}

func (c *Catalog) Close() error {
	if c == nil || c.root == nil {
		return nil
	}
	return c.root.Close()
}

type Stream struct {
	Index    int
	Type     string
	Codec    string
	Language string `json:",omitempty"`
	Title    string `json:",omitempty"`
	// Profile, Level and PixelFormat drive a client's direct play decision.
	Profile           string  `json:",omitempty"`
	Level             float64 `json:",omitempty"`
	CodecTag          string  `json:",omitempty"`
	PixelFormat       string  `json:",omitempty"`
	TimeBase          string  `json:",omitempty"`
	Width             int     `json:",omitempty"`
	Height            int     `json:",omitempty"`
	BitDepth          int     `json:",omitempty"`
	RefFrames         int     `json:",omitempty"`
	AverageFrameRate  float64 `json:",omitempty"`
	RealFrameRate     float64 `json:",omitempty"`
	Channels          int     `json:",omitempty"`
	ChannelLayout     string  `json:",omitempty"`
	SampleRate        int     `json:",omitempty"`
	BitRate           int64   `json:",omitempty"`
	IsInterlaced      bool
	IsDefault         bool
	IsForced          bool
	IsHearingImpaired bool
}

func stableID(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:16])
}

// Scan probes files sequentially. No metadata is written to the media directory.
func Scan(ctx context.Context, directory string) (*Catalog, error) {
	probe, err := newProbe(ctx)
	if err != nil {
		return nil, err
	}
	return scan(ctx, directory, probe)
}

func scan(ctx context.Context, directory string, probe func(context.Context, *os.File) (Item, error)) (*Catalog, error) {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return nil, errors.New("invalid media directory")
	}
	root, err := os.OpenRoot(absolute)
	if err != nil {
		return nil, errors.New("cannot open media directory")
	}
	catalog := &Catalog{ID: stableID("movies\x00" + absolute), Items: []Item{}, root: root}
	visit := func(path string, entry fs.DirEntry) error {
		ext := strings.ToLower(filepath.Ext(path))
		switch ext {
		case ".mp4", ".m4v", ".mkv", ".webm", ".avi", ".mov", ".mpg", ".mpeg", ".ts", ".m2ts", ".wmv", ".ogv":
		default:
			return nil
		}
		if len(catalog.Items) >= maxMovies {
			return errors.New("media directory exceeds movie limit")
		}
		// Root confines symlink races; NONBLOCK prevents a swapped-in FIFO from hanging.
		file, err := root.OpenFile(filepath.FromSlash(path), os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			catalog.Skipped++
			return nil
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() {
			catalog.Skipped++
			return nil
		}
		item, err := probe(ctx, file)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			catalog.Skipped++
			return nil
		}
		item.ID = stableID(catalog.ID + "\x00" + path)
		item.Path = path
		item.Name = strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		item.Size, item.Modified = info.Size(), info.ModTime().UTC()
		item.Container = strings.TrimPrefix(ext, ".")
		catalog.Items = append(catalog.Items, item)
		return nil
	}
	entries := 0
	var walk func(string, int) error
	walk = func(relative string, depth int) error {
		if depth > 64 {
			return errors.New("media directory exceeds scan depth")
		}
		directory, err := root.OpenFile(filepath.FromSlash(relative), os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_DIRECTORY, 0)
		if err != nil {
			return errors.New("cannot read media directory")
		}
		defer directory.Close()
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			// Avoid reading and sorting an arbitrarily large directory into memory.
			batch, readErr := directory.ReadDir(128)
			if readErr != nil && readErr != io.EOF {
				return errors.New("cannot read media directory entries")
			}
			for _, entry := range batch {
				if err := ctx.Err(); err != nil {
					return err
				}
				entries++
				if entries > maxEntries {
					return errors.New("media directory exceeds entry limit")
				}
				relativePath := path.Join(relative, entry.Name())
				if entry.IsDir() {
					if err := walk(relativePath, depth+1); err != nil {
						return err
					}
				} else if entry.Type().IsRegular() {
					if err := visit(relativePath, entry); err != nil {
						return err
					}
				}
			}
			if readErr == io.EOF {
				return nil
			}
		}
	}
	err = walk(".", 0)
	if err != nil {
		_ = catalog.Close()
		return nil, err
	}
	slices.SortFunc(catalog.Items, func(a, b Item) int { return strings.Compare(a.ID, b.ID) })
	catalog.groupEpisodes()
	return catalog, nil
}
