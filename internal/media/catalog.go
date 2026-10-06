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
	"os/exec"
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
	// extractor delivers embedded text subtitles; nil without FFmpeg.
	extractor *subtitleExtractor
	probe     func(context.Context, *os.File) (Item, error)
	index     catalogIndex
}

// Item is a movie, series, season, or episode in the catalogue.
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
	Path      string
	Container string
	Size      int64
	Modified  time.Time
	// Added is when Coach first saw the path: the file's mtime for files
	// present at startup, the rescan time for files found later.
	Added        time.Time
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

// Close releases the catalogue root. Items can no longer be opened afterwards.
func (c *Catalog) Close() error {
	if c == nil || c.root == nil {
		return nil
	}
	return c.root.Close()
}

// Stream is an audio, video, or subtitle track reported by FFprobe.
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
	// Path is set for an external subtitle file, relative to the media
	// directory. It is never sent to clients.
	Path string `json:",omitempty"`
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
	catalog, err := scan(ctx, directory, probe)
	if err != nil {
		return nil, err
	}
	// FFmpeg is optional: without it only sidecar subtitles are delivered.
	// The extractor and its job limit are shared with rescanned catalogs.
	if binary, err := exec.LookPath("ffmpeg"); err == nil {
		catalog.extractor = newSubtitleExtractor(binary)
	}
	return catalog, nil
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
	catalog, err := walk(ctx, &Catalog{ID: stableID("movies\x00" + absolute), root: root, probe: probe}, nil, time.Time{})
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	return catalog, nil
}

// Rescan walks the media directory again and returns a new catalog; c is not
// changed. Files with the same path, size and mtime reuse c's probe results,
// and every known path keeps its ID and date added. On error, including a
// walk that finds no videos where c had some (an unmounted volume looks like
// that), the caller keeps c. The catalogs share one directory root: close
// only the last one.
func (c *Catalog) Rescan(ctx context.Context) (*Catalog, error) {
	previous := make(map[string]Item, len(c.Items))
	for _, item := range c.Items {
		previous[item.Path] = item
	}
	next, err := walk(ctx, c, previous, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if len(next.Items) == 0 && len(c.Items) > 0 {
		return nil, errors.New("media directory has no videos; keeping the previous catalog")
	}
	return next, nil
}

// walk scans base's root into a new catalog. previous is nil for the first
// scan; otherwise new paths are dated now.
func walk(ctx context.Context, base *Catalog, previous map[string]Item, now time.Time) (*Catalog, error) {
	root, probe := base.root, base.probe
	catalog := &Catalog{ID: base.ID, Items: []Item{}, root: root, probe: probe, extractor: base.extractor}
	subtitles := []string{}
	visit := func(path string, entry fs.DirEntry) error {
		ext := strings.ToLower(filepath.Ext(path))
		if sidecarCodec(ext) != "" {
			subtitles = append(subtitles, path)
			return nil
		}
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
		defer func() { _ = file.Close() }()
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() {
			catalog.Skipped++
			return nil
		}
		modified := info.ModTime().UTC()
		var item Item
		if known, ok := previous[path]; ok && known.Size == info.Size() && known.Modified.Equal(modified) {
			// Sidecars and episode fields are derived again below.
			item = Item{RunTimeTicks: known.RunTimeTicks, Streams: embeddedStreams(known.Streams)}
		} else {
			item, err = probe(ctx, file)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				catalog.Skipped++
				return nil
			}
		}
		item.ID = stableID(catalog.ID + "\x00" + path)
		item.Path = path
		item.Name = strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		item.Size, item.Modified = info.Size(), modified
		switch known, ok := previous[path]; {
		case ok:
			item.Added = known.Added
		case previous == nil:
			item.Added = modified
		default:
			item.Added = now
		}
		item.Container = strings.TrimPrefix(ext, ".")
		catalog.Items = append(catalog.Items, item)
		return nil
	}
	entries := 0
	var walkDir func(string, int) error
	walkDir = func(relative string, depth int) error {
		if depth > 64 {
			return errors.New("media directory exceeds scan depth")
		}
		directory, err := root.OpenFile(filepath.FromSlash(relative), os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_DIRECTORY, 0)
		if err != nil {
			return errors.New("cannot read media directory")
		}
		defer func() { _ = directory.Close() }()
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
					if err := walkDir(relativePath, depth+1); err != nil {
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
	if err := walkDir(".", 0); err != nil {
		return nil, err
	}
	slices.SortFunc(catalog.Items, func(a, b Item) int { return strings.Compare(a.ID, b.ID) })
	attachSidecars(catalog.Items, subtitles)
	catalog.groupEpisodes()
	return catalog, nil
}
