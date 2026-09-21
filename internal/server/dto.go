package server

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/TokenCemetery/coach/internal/media"
	"github.com/TokenCemetery/coach/internal/state"
)

// Emby serialises dates with seven fractional digits and a literal Z. Go would
// read a trailing "Z" in the layout as a zone marker, so it is appended instead.
const embyDateLayout = "2006-01-02T15:04:05.0000000"

// Emby reports "no date" as .NET's DateTime.MinValue rather than omitting the field.
const zeroDate = "0001-01-01T00:00:00.0000000Z"

func embyDate(t time.Time) string { return t.UTC().Format(embyDateLayout) + "Z" }

// derive produces a stable secondary identifier from an item ID.
//
// In the captured responses a collection folder's Guid, PresentationUniqueKey
// and DisplayPreferencesId all hold the same value, so Coach emits one derived
// value for the three. They are opaque to clients; only stability matters.
func derive(purpose, id string) string {
	sum := sha256.Sum256([]byte(purpose + "\x00" + id))
	return hex.EncodeToString(sum[:16])
}

// baseFields carries the properties Emby returns on every item. Emby Web
// dereferences several of these arrays without a guard, so they are always
// emitted even when empty; omitting them crashes the client.
func baseFields(id, name, kind, serverID string) object {
	guid := derive("guid", id)
	return object{
		"Name":                  name,
		"ServerId":              serverID,
		"Id":                    id,
		"Guid":                  guid,
		"Etag":                  derive("etag", id),
		"DateModified":          zeroDate,
		"CanDelete":             false,
		"CanDownload":           false,
		"PresentationUniqueKey": guid,
		"SupportsSync":          false,
		"SortName":              name,
		"ExternalUrls":          []any{},
		"Taglines":              []string{},
		"RemoteTrailers":        []any{},
		"ProviderIds":           object{},
		"Type":                  kind,
		"DisplayPreferencesId":  guid,
		"ImageTags":             object{},
		"BackdropImageTags":     []string{},
		"LockedFields":          []string{},
		"LockData":              false,
	}
}

// folderUserData matches Emby's collection folders, which omit PlayCount.
func folderUserData() object {
	return object{"PlaybackPositionTicks": 0, "IsFavorite": false, "Played": false}
}

// itemUserData reports stored playback state. PlayedPercentage is only present
// when there is a position to report, matching the reference responses.
func itemUserData(st state.ItemState, runtime int64) object {
	data := object{"PlaybackPositionTicks": st.PositionTicks, "PlayCount": st.PlayCount,
		"IsFavorite": st.IsFavorite, "Played": st.Played}
	if runtime > 0 && st.PositionTicks > 0 {
		data["PlayedPercentage"] = float64(st.PositionTicks) / float64(runtime) * 100
	}
	if !st.LastPlayed.IsZero() {
		data["LastPlayedDate"] = embyDate(st.LastPlayed)
	}
	return data
}

// resumable mirrors Emby's rule for the Continue Watching row: enough progress
// to be meaningful, but not so much that the item is effectively finished.
//
// A stored position governs this, not the played flag: rewatching an item
// already marked played must put it back in the row, and finishing one clears
// its position so it drops out.
func resumable(st state.ItemState, runtime int64) bool {
	if st.PositionTicks <= 0 || runtime <= 0 {
		return false
	}
	percent := float64(st.PositionTicks) / float64(runtime) * 100
	return percent >= 1 && percent <= 90
}

// rootFolderDTO is the UserRootFolder returned by /Users/{id}/Items/Root.
func (s *Server) rootFolderDTO(serverID string) object {
	dto := baseFields(rootID, "Media Folders", "UserRootFolder", serverID)
	dto["DateCreated"] = zeroDate
	dto["IsFolder"] = true
	dto["UserData"] = folderUserData()
	dto["ChildCount"] = s.libraryCount()
	return dto
}

// libraryDTO is the CollectionFolder for Coach's single movie library.
//
// detail selects the richer body Emby returns from /Users/{id}/Items/{id}. Only
// that response carries Subviews, and Emby Web's library screen dereferences it
// without a guard (videos.js: this.item.Subviews.includes(...)), so a library
// served without Subviews breaks the screen outright.
func (s *Server) libraryDTO(serverID string, detail bool) object {
	return s.collectionDTO(s.media.ID, serverID, detail)
}

func (s *Server) collectionDTO(id, serverID string, detail bool) object {
	name, collectionType, subview := "Movies", "movies", "movies"
	if id == s.media.SeriesLibraryID() {
		name, collectionType, subview = "TV Shows", "tvshows", "series"
	}
	dto := baseFields(id, name, "CollectionFolder", serverID)
	dto["DateCreated"] = zeroDate
	dto["IsFolder"] = true
	dto["ParentId"] = rootID
	dto["CollectionType"] = collectionType
	dto["UserData"] = folderUserData()
	dto["PrimaryImageAspectRatio"] = 1.7777777777777777
	if detail {
		dto["ChildCount"] = s.childCount(id)
		// Only the subviews Coach can actually serve are advertised.
		dto["Subviews"] = []string{subview}
	}
	return dto
}

func (s *Server) libraryCount() int {
	return len(s.libraryIDs())
}

func (s *Server) libraryIDs() []string {
	if s.media == nil {
		return nil
	}
	if len(s.media.Folders) > 0 {
		return []string{s.media.ID, s.media.SeriesLibraryID()}
	}
	return []string{s.media.ID}
}

func (s *Server) childCount(id string) int {
	count := 0
	for _, items := range [][]media.Item{s.media.Items, s.media.Folders} {
		for _, item := range items {
			if s.media.Parent(item) == id {
				count++
			}
		}
	}
	return count
}

func (s *Server) movieDTO(item media.Item, serverID string, items map[string]state.ItemState) object {
	dto := baseFields(item.ID, item.Name, item.Type(), serverID)
	dto["DateCreated"] = embyDate(item.Modified)
	dto["DateModified"] = embyDate(item.Modified)
	dto["IsFolder"] = item.IsFolder()
	dto["ParentId"] = s.media.Parent(item)
	dto["GenreItems"] = []any{}
	dto["TagItems"] = []any{}
	dto["UserData"] = itemUserData(items[item.ID], item.RunTimeTicks)
	if item.SeriesID != "" {
		dto["SeriesId"], dto["SeriesName"] = item.SeriesID, item.SeriesName
	}
	if item.Type() == "Season" {
		dto["IndexNumber"] = item.SeasonNumber
	}
	if item.Type() == "Episode" {
		dto["IndexNumber"], dto["ParentIndexNumber"] = item.EpisodeNumber, item.SeasonNumber
		dto["SeasonId"], dto["SeasonName"] = item.SeasonID, "Season "+itoa(item.SeasonNumber)
	}
	if item.IsFolder() {
		dto["ChildCount"] = s.childCount(item.ID)
		return dto
	}
	dto["MediaType"] = "Video"
	dto["VideoType"] = "VideoFile"
	dto["Container"] = item.Container
	dto["Size"] = item.Size
	dto["PartCount"] = 1
	dto["LocalTrailerCount"] = 0
	dto["GenreItems"] = []any{}
	dto["TagItems"] = []any{}
	dto["UserData"] = itemUserData(items[item.ID], item.RunTimeTicks)
	dto["CanDownload"] = true
	if mime := containerMIME(item.Container); mime != "" {
		dto["MimeType"] = mime
	}
	if item.RunTimeTicks > 0 {
		dto["RunTimeTicks"] = item.RunTimeTicks
	}
	streams := mediaStreams(item)
	dto["MediaStreams"] = streams
	dto["MediaSources"] = []object{mediaSource(item, streams)}
	if w, h := videoSize(item); w > 0 && h > 0 {
		dto["Width"], dto["Height"] = w, h
		dto["PrimaryImageAspectRatio"] = float64(w) / float64(h)
	}
	return dto
}

func videoSize(item media.Item) (int, int) {
	for _, stream := range item.Streams {
		if stream.Type == "Video" {
			return stream.Width, stream.Height
		}
	}
	return 0, 0
}

// containerMIME covers the containers media.Scan accepts. An unknown container
// yields no MimeType rather than a guess the client would act on.
func containerMIME(container string) string {
	switch strings.ToLower(container) {
	case "mp4", "m4v":
		return "video/mp4"
	case "mkv":
		return "video/x-matroska"
	case "webm":
		return "video/webm"
	case "avi":
		return "video/x-msvideo"
	case "mov":
		return "video/quicktime"
	case "mpg", "mpeg":
		return "video/mpeg"
	case "ts", "m2ts":
		return "video/mp2t"
	case "wmv":
		return "video/x-ms-wmv"
	case "ogv":
		return "video/ogg"
	}
	return ""
}

// mediaStreams maps probe output onto Emby's MediaStream. Clients decide direct
// play from Profile, Level, PixelFormat and the frame rates, so these are
// emitted whenever ffprobe reported them.
func mediaStreams(item media.Item) []object {
	streams := make([]object, 0, len(item.Streams))
	for _, s := range item.Streams {
		stream := object{
			"Index":                           s.Index,
			"Type":                            s.Type,
			"Codec":                           s.Codec,
			"IsDefault":                       s.IsDefault,
			"IsForced":                        s.IsForced,
			"IsHearingImpaired":               s.IsHearingImpaired,
			"IsExternal":                      false,
			"IsInterlaced":                    s.IsInterlaced,
			"IsTextSubtitleStream":            s.Type == "Subtitle",
			"SupportsExternalStream":          false,
			"Protocol":                        "File",
			"DisplayTitle":                    streamTitle(s),
			"ExtendedVideoType":               "None",
			"ExtendedVideoSubType":            "None",
			"ExtendedVideoSubTypeDescription": "None",
			"AttachmentSize":                  0,
		}
		for key, value := range map[string]string{"Language": s.Language, "Title": s.Title,
			"CodecTag": s.CodecTag, "TimeBase": s.TimeBase, "Profile": s.Profile} {
			if value != "" {
				stream[key] = value
			}
		}
		if s.BitRate > 0 {
			stream["BitRate"] = s.BitRate
		}
		switch s.Type {
		case "Video":
			stream["Width"], stream["Height"] = s.Width, s.Height
			stream["VideoRange"] = "SDR"
			stream["Level"] = s.Level
			stream["IsAnamorphic"] = false
			stream["IsAVC"] = strings.EqualFold(s.Codec, "h264")
			if s.PixelFormat != "" {
				stream["PixelFormat"] = s.PixelFormat
			}
			if s.BitDepth > 0 {
				stream["BitDepth"] = s.BitDepth
			}
			if s.RefFrames > 0 {
				stream["RefFrames"] = s.RefFrames
			}
			if s.AverageFrameRate > 0 {
				stream["AverageFrameRate"] = s.AverageFrameRate
			}
			if s.RealFrameRate > 0 {
				stream["RealFrameRate"] = s.RealFrameRate
			}
			if s.Height > 0 {
				stream["AspectRatio"] = aspectRatio(s.Width, s.Height)
			}
		case "Audio":
			stream["Channels"] = s.Channels
			if s.ChannelLayout != "" {
				stream["ChannelLayout"] = s.ChannelLayout
			}
			if s.SampleRate > 0 {
				stream["SampleRate"] = s.SampleRate
			}
		}
		streams = append(streams, stream)
	}
	return streams
}

func streamTitle(s media.Stream) string {
	parts := []string{}
	if s.Language != "" {
		parts = append(parts, s.Language)
	}
	if s.Codec != "" {
		parts = append(parts, strings.ToUpper(s.Codec))
	}
	if s.Type == "Video" && s.Height > 0 {
		parts = append(parts, verticalResolution(s.Height))
	}
	if s.Type == "Audio" && s.Channels > 0 {
		parts = append(parts, channelLayout(s.Channels))
	}
	return strings.Join(parts, " ")
}

func verticalResolution(height int) string {
	switch {
	case height >= 2000:
		return "4K"
	case height >= 1000:
		return "1080p"
	case height >= 700:
		return "720p"
	case height >= 500:
		return "576p"
	}
	return "480p"
}

func channelLayout(channels int) string {
	switch channels {
	case 1:
		return "Mono"
	case 2:
		return "Stereo"
	case 6:
		return "5.1"
	case 8:
		return "7.1"
	}
	return "Multichannel"
}

func aspectRatio(width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	a, b := width, height
	for b != 0 {
		a, b = b, a%b
	}
	if a == 0 {
		return ""
	}
	return itoa(width/a) + ":" + itoa(height/a)
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	digits := []byte{}
	for v > 0 {
		digits = append([]byte{byte('0' + v%10)}, digits...)
		v /= 10
	}
	return string(digits)
}

// mediaSource describes the single local file backing an item.
//
// SupportsDirectPlay is deliberately false. In Emby's model Direct Play means
// the client opens MediaSource.Path on the filesystem itself, and Emby Web's
// createStreamInfo then uses that path as the media URL; a browser cannot do
// that, and a server that claims it without sending Path leaves the client with
// no URL at all ("no compatible streams"). Serving the unmodified file over
// HTTP is Direct Stream, which is what Coach does. Coach never transcodes, so
// that is reported as unsupported rather than advertised and then refused.
func mediaSource(item media.Item, streams []object) object {
	source := object{
		"Id":                    "mediasource_" + item.ID,
		"Protocol":              "File",
		"Type":                  "Default",
		"Container":             item.Container,
		"Size":                  item.Size,
		"Name":                  item.Name,
		"IsRemote":              false,
		"HasMixedProtocols":     false,
		"SupportsTranscoding":   false,
		"SupportsDirectStream":  true,
		"SupportsDirectPlay":    false,
		"IsInfiniteStream":      false,
		"RequiresOpening":       false,
		"RequiresClosing":       false,
		"RequiresLooping":       false,
		"SupportsProbing":       true,
		"MediaStreams":          streams,
		"Chapters":              []any{},
		"ReadAtNativeFramerate": false,
	}
	if item.RunTimeTicks > 0 {
		source["RunTimeTicks"] = item.RunTimeTicks
	}
	if mime := containerMIME(item.Container); mime != "" {
		source["MimeType"] = mime
	}
	return source
}

// defaultAudioStreamIndex reports the audio track a client should start with.
func defaultAudioStreamIndex(item media.Item) (int, bool) {
	fallback, found := 0, false
	for _, s := range item.Streams {
		if s.Type != "Audio" {
			continue
		}
		if s.IsDefault {
			return s.Index, true
		}
		if !found {
			fallback, found = s.Index, true
		}
	}
	return fallback, found
}
