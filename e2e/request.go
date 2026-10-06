package e2e

import (
	"fmt"
	"net/url"
	"strings"
)

// Request is a concrete HTTP request built for an operation.
type Request struct {
	Method      string
	Path        string
	Query       url.Values
	Body        []byte
	ContentType string
}

// URL returns the request target under the Emby-compatible /emby prefix,
// except for Coach-only routes served at the root.
func (r Request) URL(base string) string {
	prefix := "/emby"
	if r.Path == "/healthz" {
		prefix = ""
	}
	u := base + prefix + r.Path
	if len(r.Query) > 0 {
		u += "?" + r.Query.Encode()
	}
	return u
}

// requestOverrides replace generated bodies where an empty value cannot
// produce a successful response.
var requestOverrides = map[string]func(*Request, Fixtures){
	"postUsersAuthenticatebyname": func(r *Request, _ Fixtures) {
		r.Body = []byte(`{"Username":"` + testUser + `","Pw":"` + testPassword + `"}`)
	},
	"postSessionsPlaying":         playstateBody,
	"postSessionsPlayingProgress": playstateBody,
	"postSessionsPlayingStopped":  playstateBody,
	// The Profile page sends the current password with the new one.
	"postUsersByIdPassword": func(r *Request, _ Fixtures) {
		r.Body = []byte(`{"CurrentPw":"` + testPassword + `","NewPw":"e2e-only-new-password"}`)
	},
	"postUsersByIdImagesByType":        avatarUpload,
	"postUsersByIdImagesByTypeByIndex": avatarUpload,
	// Emby Web always reports at least one item and WasSearched.
	"postUsersByUseridSearcheditems": func(r *Request, ids Fixtures) {
		r.Body = fmt.Appendf(nil, `{"Ids":[%q],"WasSearched":true}`, ids.MovieID)
	},
}

// avatarUpload sends an image the way Emby Web does: base64 text, not bytes.
func avatarUpload(r *Request, _ Fixtures) {
	r.Body = avatarBody()
}

// playstateBody reports playback of the movie, which the playstate
// operations require to find the item.
func playstateBody(r *Request, ids Fixtures) {
	r.Body = fmt.Appendf(nil, `{"ItemId":%q,"MediaSourceId":%q,"PlaySessionId":"e2e","PositionTicks":0}`, ids.MovieID, ids.MediaSourceID)
}

// BuildRequest fills required path, query and body inputs of op with values
// that refer to the instance's fixtures. Optional parameters are omitted.
func (c *Contract) BuildRequest(op Operation, ids Fixtures) Request {
	r := Request{Method: op.Method, Path: op.Path, Query: url.Values{}}
	for _, p := range op.Params {
		switch {
		case p.In == "path":
			r.Path = strings.ReplaceAll(r.Path, "{"+p.Name+"}", url.PathEscape(c.value(p, op.Path, ids)))
		case p.In == "query" && p.Required:
			r.Query.Set(p.Name, c.value(p, op.Path, ids))
		}
	}
	if op.Body != nil {
		r.ContentType = op.Body.ContentType
		if op.Body.ContentType == "application/json" {
			r.Body = []byte(c.emptyJSON(op.Body.Schema))
		}
	}
	if override, ok := requestOverrides[op.ID]; ok {
		override(&r, ids)
	}
	return r
}

// value picks a parameter value: a fixture ID when the name refers to an
// entity, otherwise the first enum member or a neutral literal.
func (c *Contract) value(p Param, path string, ids Fixtures) string {
	switch p.Name {
	case "UserId":
		return ids.UserID
	case "Id", "ItemId", "Ids", "ItemIds", "EntryIds":
		return entityFor(path, ids)
	case "MediaSourceId":
		return ids.MediaSourceID
	case "TargetId", "SessionId":
		return ids.SessionID
	case "DeviceId":
		return ids.DeviceID
	case "ParentId":
		return ids.LibraryID
	case "SectionId":
		return "resume"
	case "Index":
		if strings.Contains(path, "/Subtitles/") && ids.SubtitleIndex != "" {
			return ids.SubtitleIndex
		}
		return "0"
	case "Format":
		if strings.Contains(path, "/Subtitles/") {
			return "vtt"
		}
		return "jpg"
	case "Container":
		return "mp4"
	case "StreamFileName":
		return "stream.mp4"
	case "SegmentContainer":
		return "ts"
	case "SegmentId":
		return "0"
	case "Type":
		if strings.Contains(path, "/Images/") {
			return "Primary"
		}
	case "DisplayPreferencesId":
		return "usersettings"
	case "Path":
		return "/"
	}
	s := p.Schema
	if ref, ok := s["$ref"].(string); ok {
		if resolved, err := c.resolve(ref); err == nil {
			s = resolved
		}
	}
	if enum := list(s["enum"]); len(enum) > 0 {
		return str(enum[0])
	}
	switch str(s["type"]) {
	case "integer", "number":
		return "0"
	case "boolean":
		return "false"
	}
	return "e2e"
}

// entityFor chooses the item an {Id} refers to from the route it appears in.
func entityFor(path string, ids Fixtures) string {
	switch {
	case strings.HasPrefix(path, "/Users/{Id}"):
		return ids.UserID
	case strings.HasPrefix(path, "/Sessions/{Id}"):
		return ids.SessionID
	case strings.HasPrefix(path, "/Shows/"):
		return ids.SeriesID
	}
	return ids.MovieID
}

// emptyJSON is the smallest value of the schema's type. The contract declares
// no required properties, so an empty object is a valid body.
func (c *Contract) emptyJSON(schema map[string]any) string {
	if ref, ok := schema["$ref"].(string); ok {
		if resolved, err := c.resolve(ref); err == nil {
			schema = resolved
		}
	}
	switch str(schema["type"]) {
	case "array":
		return "[]"
	case "string":
		return `""`
	}
	return "{}"
}
