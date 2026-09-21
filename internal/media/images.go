package media

import (
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path"
	"strings"
)

const maxImageBytes = 8 << 20

type Image struct {
	Revision      string
	Width, Height int
	MIME          string
}

// OpenImage searches local JPEG/PNG sidecars. Headers and encoded file size are
// bounded; pixels are never decoded or resized in the server.
func (c *Catalog) OpenImage(item Item) (*os.File, Image, error) {
	if c == nil || c.root == nil {
		return nil, Image{}, os.ErrNotExist
	}
	bases := []string{}
	dir := path.Dir(item.Path)
	if item.IsFolder() {
		dir = item.Path
	} else {
		stem := strings.TrimSuffix(item.Path, path.Ext(item.Path))
		bases = append(bases, stem+"-poster", stem)
	}
	bases = append(bases, path.Join(dir, "poster"), path.Join(dir, "folder"))
	for _, base := range bases {
		for _, ext := range []string{".jpg", ".jpeg", ".png"} {
			file, err := c.Open(Item{Path: base + ext})
			if err != nil {
				continue
			}
			info, err := file.Stat()
			if err != nil || info.Size() <= 0 || info.Size() > maxImageBytes {
				file.Close()
				continue
			}
			config, format, err := image.DecodeConfig(io.LimitReader(file, 1<<20))
			if err != nil || (format != "jpeg" && format != "png") || config.Width <= 0 || config.Height <= 0 || config.Width > 16384 || config.Height > 16384 || int64(config.Width)*int64(config.Height) > 32_000_000 {
				file.Close()
				continue
			}
			if _, err := file.Seek(0, io.SeekStart); err != nil {
				file.Close()
				continue
			}
			return file, Image{Revision: stableID(fmt.Sprintf("%s\x00%d\x00%d", base+ext, info.Size(), info.ModTime().UnixNano())), Width: config.Width, Height: config.Height, MIME: "image/" + format}, nil
		}
	}
	return nil, Image{}, os.ErrNotExist
}

func (c *Catalog) PrimaryImage(item Item) (Image, bool) {
	file, info, err := c.OpenImage(item)
	if err != nil {
		return Image{}, false
	}
	file.Close()
	return info, true
}
