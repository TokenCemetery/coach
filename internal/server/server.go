package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/TokenCemetery/coach/internal/media"
	"github.com/TokenCemetery/coach/internal/state"
)

// CompatibilityVersion selects the API branch used by the reference Emby Web.
// It is not Coach's product version or a claim of full Emby compatibility.
const CompatibilityVersion = "4.10.0.40"
const Version = "0.1.0"

type object map[string]any

type Server struct {
	store       *state.Store
	name        string
	web         http.Handler
	logins      loginLimit
	media       *media.Catalog
	sockets     atomic.Int64
	socketMu    sync.Mutex
	connections map[*socket]struct{}
	closing     bool
	itemMu      sync.Mutex
}

func New(store *state.Store, name string, web http.Handler, catalog *media.Catalog) *Server {
	return &Server{store: store, name: name, web: web, media: catalog, logins: loginLimit{active: make(chan struct{}, 2)}, connections: make(map[*socket]struct{})}
}

func respond(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func fail(w http.ResponseWriter, status int, code string) {
	respond(w, status, object{"ResponseStatus": object{"ErrorCode": code, "Message": code}})
}

func (s *Server) internalError(w http.ResponseWriter) {
	// Do not print request URLs (query tokens), bodies, or credentials.
	slog.Error("state persistence failed")
	fail(w, 500, "StorageError")
}

func (s *Server) publicInfo() object {
	d := s.store.Snapshot()
	return object{"LocalAddresses": []string{}, "RemoteAddresses": []string{}, "ServerName": s.name, "Version": CompatibilityVersion, "Id": d.ServerID}
}

func publicUser(d state.Data) object {
	return object{"Name": d.User.Name, "ServerId": d.ServerID, "Prefix": "", "Id": d.User.ID, "HasPassword": true, "HasConfiguredPassword": true}
}

func userDTO(d state.Data) object {
	u := publicUser(d)
	config := defaultConfiguration()
	for k, v := range d.User.Configuration {
		config[k] = v
	}
	u["Configuration"] = config
	u["Policy"] = policy()
	return u
}

// policy mirrors the reference policy shape with values that match what Coach
// can actually do: direct playback and downloading of the original file, no
// transcoding, remuxing, Live TV, sync or remote control. Emby Web refuses to
// start playback when EnableMediaPlayback is false, so this is load-bearing.
func policy() object {
	return object{
		"IsAdministrator": false, "IsHidden": false, "IsHiddenRemotely": true,
		"IsHiddenFromUnusedDevices": false, "IsDisabled": false, "LockedOutDate": 0,
		"AllowTagOrRating": false, "BlockedTags": []string{}, "IsTagBlockingModeInclusive": false,
		"IncludeTags": []string{}, "EnableUserPreferenceAccess": true, "AccessSchedules": []any{},
		"BlockUnratedItems": []string{}, "EnableRemoteControlOfOtherUsers": false,
		"EnableSharedDeviceControl": false, "EnableRemoteAccess": true,
		"EnableLiveTvManagement": false, "EnableLiveTvAccess": false,
		"EnableMediaPlayback": true,
		// Coach serves the stored file unchanged; it never spawns an encoder.
		"EnableAudioPlaybackTranscoding": false, "EnableVideoPlaybackTranscoding": false,
		"EnableTranscodingQuality": false, "AutoRemoteQuality": 0, "EnablePlaybackRemuxing": false,
		"EnableContentDeletion": false, "RestrictedFeatures": []string{},
		"EnableContentDeletionFromFolders": []string{}, "EnableContentDownloading": true,
		"EnableSubtitleDownloading": false, "EnableSubtitleManagement": false,
		"EnableSyncTranscoding": false, "EnableMediaConversion": false,
		"EnabledChannels": []string{}, "EnableAllChannels": false,
		"EnabledFolders": []string{}, "EnableAllFolders": true,
		"InvalidLoginAttemptCount": 0, "EnablePublicSharing": false,
		"RemoteClientBitrateLimit": 0, "ExcludedSubFolders": []string{},
		"SimultaneousStreamLimit": 0, "EnabledDevices": []string{}, "EnableAllDevices": true,
		"AllowCameraUpload": false, "AllowSharingPersonalItems": false,
	}
}

func defaultConfiguration() object {
	return object{"PlayDefaultAudioTrack": true, "SubtitleLanguagePreference": "", "DisplayMissingEpisodes": false, "GroupedFolders": []string{}, "OrderedViews": []string{}, "LatestItemsExcludes": []string{}, "MyMediaExcludes": []string{}, "HidePlayedInLatest": true, "RememberAudioSelections": true, "RememberSubtitleSelections": true, "EnableNextEpisodeAutoPlay": true, "SubtitleMode": "Default"}
}

func sessionDTO(d state.Data, session state.Session) object {
	caps := object{"PlayableMediaTypes": []string{}, "SupportedCommands": []string{}, "SupportsMediaControl": false, "SupportsPersistentIdentifier": false}
	for k, v := range session.Capabilities {
		caps[k] = v
	}
	return object{"Id": session.ID, "ServerId": d.ServerID, "UserId": session.UserID, "UserName": d.User.Name, "Client": session.Client, "DeviceId": session.DeviceID, "DeviceName": session.DeviceName, "ApplicationVersion": session.Version, "AdditionalUsers": []any{}, "PlayState": object{"IsPaused": false, "IsMuted": false}, "Capabilities": caps, "SupportsRemoteControl": false}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		respond(w, 200, object{"status": "ok", "version": Version})
	})
	mux.HandleFunc("GET /system/info/public", func(w http.ResponseWriter, r *http.Request) { respond(w, 200, s.publicInfo()) })
	ping := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("Emby Server"))
	}
	mux.HandleFunc("GET /system/ping", ping)
	mux.HandleFunc("POST /system/ping", ping)
	mux.HandleFunc("GET /users/public", func(w http.ResponseWriter, r *http.Request) { respond(w, 200, []any{publicUser(s.store.Snapshot())}) })
	mux.HandleFunc("GET /branding/configuration", func(w http.ResponseWriter, r *http.Request) {
		respond(w, 200, object{"LoginDisclaimer": "", "CustomCss": ""})
	})
	// Emby Web loads this as a stylesheet element, so the ".css" spelling must
	// answer with a CSS content type or the browser refuses the response.
	branding := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		w.WriteHeader(200)
	}
	mux.HandleFunc("GET /branding/css", branding)
	mux.HandleFunc("GET /branding/css.css", branding)
	mux.HandleFunc("POST /users/authenticatebyname", s.login)
	mux.HandleFunc("GET /system/info", s.protect(func(w http.ResponseWriter, r *http.Request, token string, session state.Session) {
		info := s.publicInfo()
		info["ProductName"], info["CoachVersion"] = "Coach", Version
		info["SupportsLibraryMonitor"], info["SupportsLocalPortConfiguration"], info["SupportsHttps"] = false, false, false
		respond(w, 200, info)
	}))
	mux.HandleFunc("GET /system/endpoint", s.protect(func(w http.ResponseWriter, r *http.Request, token string, session state.Session) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		ip := net.ParseIP(host)
		local, lan := ip.IsLoopback(), ip.IsLoopback() || ip.IsPrivate()
		network := "wan"
		if lan {
			network = "lan"
		}
		respond(w, 200, object{"IsLocal": local, "IsInNetwork": lan, "NetworkType": network})
	}))
	user := s.protect(func(w http.ResponseWriter, r *http.Request, token string, session state.Session) {
		respond(w, 200, userDTO(s.store.Snapshot()))
	})
	mux.HandleFunc("GET /users/{user}", user)
	mux.HandleFunc("GET /users/me", user)
	mux.HandleFunc("POST /sessions/logout", s.protect(func(w http.ResponseWriter, r *http.Request, token string, session state.Session) {
		if s.store.Logout(token) != nil {
			s.internalError(w)
			return
		}
		s.disconnectSession(session.ID)
		w.WriteHeader(204)
	}))
	mux.HandleFunc("GET /sessions", s.protect(func(w http.ResponseWriter, r *http.Request, token string, session state.Session) {
		respond(w, 200, []any{sessionDTO(s.store.Snapshot(), session)})
	}))
	mux.HandleFunc("POST /sessions/capabilities/full", s.protect(func(w http.ResponseWriter, r *http.Request, token string, session state.Session) {
		var caps map[string]json.RawMessage
		if decodeJSON(w, r, &caps) != nil || caps == nil {
			fail(w, 400, "InvalidRequest")
			return
		}
		s.changed(w, s.store.Change(token, func(d *state.Data, session *state.Session) { session.Capabilities = caps }))
	}))
	for _, path := range []string{"/usersettings/{user}", "/users/{user}/configuration"} {
		configuration := strings.Contains(path, "/configuration")
		mux.HandleFunc("GET "+path, s.protect(func(w http.ResponseWriter, r *http.Request, token string, session state.Session) {
			d := s.store.Snapshot()
			if configuration {
				respond(w, 200, userDTO(d)["Configuration"])
			} else {
				respond(w, 200, d.User.Settings)
			}
		}))
		for _, suffix := range []string{"", "/partial"} {
			partial := suffix != ""
			mux.HandleFunc("POST "+path+suffix, s.protect(func(w http.ResponseWriter, r *http.Request, token string, session state.Session) {
				var settings map[string]json.RawMessage
				if decodeJSON(w, r, &settings) != nil || settings == nil {
					fail(w, 400, "InvalidRequest")
					return
				}
				// Client configuration must not null out arrays required by the web UI.
				if configuration && !validConfiguration(settings) {
					fail(w, 400, "InvalidRequest")
					return
				}
				s.changed(w, s.store.Change(token, func(d *state.Data, session *state.Session) {
					target := &d.User.Settings
					if configuration {
						target = &d.User.Configuration
					}
					if !partial || *target == nil {
						*target = map[string]json.RawMessage{}
					}
					for k, v := range settings {
						(*target)[k] = v
					}
				}))
			}))
		}
	}
	emptyPage := s.protect(func(w http.ResponseWriter, r *http.Request, token string, session state.Session) {
		respond(w, 200, object{"Items": []any{}, "TotalRecordCount": 0})
	})
	for _, path := range []string{"/shows/nextup", "/shows/upcoming", "/livetv/recordings"} {
		mux.HandleFunc("GET "+path, emptyPage)
	}
	emptyList := s.protect(func(w http.ResponseWriter, r *http.Request, token string, session state.Session) {
		respond(w, 200, []any{})
	})
	for _, path := range []string{"/features"} {
		mux.HandleFunc("GET "+path, emptyList)
	}
	// Emby Web strips the "/emby" prefix when it derives the socket address, so
	// this route is registered at the root and not under the API prefix.
	mux.HandleFunc("GET /embywebsocket", s.protect(s.websocket))
	s.catalogRoutes(mux)
	s.homeRoutes(mux)
	s.playbackRoutes(mux)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { fail(w, 404, "NotFound") })
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Coach-Version", Version)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Emby-Authorization, X-Emby-Token, X-MediaBrowser-Token, X-Emby-Client, X-Emby-Client-Version, X-Emby-Device-Id, X-Emby-Device-Name")
		if r.Method == "OPTIONS" {
			w.WriteHeader(204)
			return
		}
		if r.URL.Path == "/" && (r.Method == "GET" || r.Method == "HEAD") {
			if s.web != nil {
				http.Redirect(w, r, "/web/index.html", 302)
			} else {
				respond(w, 200, object{"ProductName": "Coach", "Version": Version, "WebClientConfigured": false})
			}
			return
		}
		if strings.HasPrefix(r.URL.Path, "/web/") {
			if s.web == nil {
				fail(w, 404, "WebClientNotConfigured")
			} else {
				s.web.ServeHTTP(w, r)
			}
			return
		}
		// Normalize only API paths; static asset paths retain their original case.
		clone := r.Clone(r.Context())
		clone.URL.Path = strings.ToLower(r.URL.Path)
		if strings.HasPrefix(clone.URL.Path, "/emby/") {
			clone.URL.Path = strings.TrimPrefix(clone.URL.Path, "/emby")
		}
		clone.URL.RawPath = ""
		mux.ServeHTTP(w, clone)
	})
}

func validConfiguration(config map[string]json.RawMessage) bool {
	defaults := defaultConfiguration()
	for k, v := range config {
		if string(v) == "null" {
			if _, known := defaults[k]; known {
				return false
			}
		}
		switch defaults[k].(type) {
		case []string:
			var a []string
			if json.Unmarshal(v, &a) != nil || a == nil {
				return false
			}
		case bool:
			var b bool
			if json.Unmarshal(v, &b) != nil {
				return false
			}
		case string:
			var s string
			if json.Unmarshal(v, &s) != nil {
				return false
			}
		}
	}
	return true
}

func (s *Server) changed(w http.ResponseWriter, err error) {
	if errors.Is(err, state.ErrStateLimit) {
		fail(w, 413, "StateLimitReached")
		return
	}
	if errors.Is(err, state.ErrSession) {
		fail(w, 401, "Unauthorized")
		return
	}
	if err != nil {
		s.internalError(w)
		return
	}
	w.WriteHeader(204)
}
