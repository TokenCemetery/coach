package server

import (
	"encoding/json"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/TokenCemetery/coach/internal/hls"
	"github.com/TokenCemetery/coach/internal/media"
)

// hlsProfile is the shape of Emby Web's browser profile: MP4 direct play and
// HLS in MPEG-TS for streaming.
const hlsProfile = `{"DirectPlayProfiles":[{"Container":"mp4","Type":"Video","VideoCodec":"h264","AudioCodec":"aac"}],
	"TranscodingProfiles":[{"Container":"ts","Type":"Video","Protocol":"hls","VideoCodec":"h264,hevc","AudioCodec":"mp3,aac"}],
	"CodecProfiles":[{"Type":"Video","Codec":"h264","Conditions":[{"Condition":"LessThanEqual","Property":"VideoLevel","Value":"51"}]}]}`

func TestPlaybackInfoRemux(t *testing.T) {
	store, _, dir := newTestServer(t)
	movie := playbackMovie()
	movie.Container = "mkv"
	catalog := &media.Catalog{ID: "movies", Items: []media.Item{movie}}
	server := New(store, "test", nil, catalog)
	m, err := hls.NewManager("ffmpeg", "ffprobe", filepath.Join(dir, "transcode"))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	server.SetHLS(m)
	h := server.Handler()
	token := login(t, h)
	info := func(query, profile string) map[string]any {
		t.Helper()
		w := request(h, "POST", "/Items/movie/PlaybackInfo?"+query, "application/json", `{"DeviceProfile":`+profile+`}`, token)
		expectStatus(t, w, 200)
		var response struct {
			ErrorCode    string
			MediaSources []map[string]any
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		source := response.MediaSources[0]
		source["ErrorCode"] = response.ErrorCode
		return source
	}
	transcoding := func(source map[string]any) url.Values {
		t.Helper()
		raw, _ := source["TranscodingUrl"].(string)
		path, query, _ := strings.Cut(raw, "?")
		values, err := url.ParseQuery(query)
		if path != "/videos/movie/master.m3u8" || err != nil || source["SupportsTranscoding"] != true || source["SupportsDirectStream"] != false ||
			source["TranscodingSubProtocol"] != "hls" || source["TranscodingContainer"] != "ts" || source["ErrorCode"] != "" {
			t.Fatalf("not a remux: %v", source)
		}
		return values
	}
	// MKV is not direct-playable: the AAC track is copied into HLS.
	q := transcoding(info("", hlsProfile))
	if q.Get("VideoCodec") != "h264" || q.Get("AudioCodec") != "aac" || q.Get("AudioStreamIndex") != "2" || q.Get("MaxAudioChannels") != "" || q.Get("api_key") != token {
		t.Fatalf("aac remux query %v", q)
	}
	// AC3 is not accepted in HLS: it is encoded to stereo AAC.
	q = transcoding(info("AudioStreamIndex=1", hlsProfile))
	if q.Get("AudioCodec") != "aac" || q.Get("AudioStreamIndex") != "1" || q.Get("MaxAudioChannels") != "2" {
		t.Fatalf("ac3 remux query %v", q)
	}
	// Video the profile rejects is encoded to H.264, capped at the source's
	// bitrate or the client's limit less room for audio.
	for name, test := range map[string]struct {
		profile, bitrate string
	}{
		"level":   {strings.Replace(hlsProfile, `"Value":"51"`, `"Value":"40"`, 1), strconv.FormatInt(bitrate(movie), 10)},
		"bitrate": {strings.Replace(hlsProfile, `{"DirectPlayProfiles"`, `{"MaxStreamingBitrate":1000000,"DirectPlayProfiles"`, 1), "808000"},
	} {
		if q := transcoding(info("", test.profile)); q.Get("VideoCodec") != "h264" || q.Get("VideoBitrate") != test.bitrate || q.Get("AudioCodec") != "aac" || q.Has("MaxWidth") {
			t.Fatalf("%s: transcoding query %v", name, q)
		}
	}
	// Emby Web limits H.264 to 1920 wide when the browser cannot decode 4K
	// smoothly; the limits of the H.264 codec profile bound the encoded video.
	width := strings.Replace(hlsProfile, `"Conditions":[`, `"Conditions":[{"Condition":"LessThanEqual","Property":"Width","Value":"1280","IsRequired":false},`+
		`{"Condition":"LessThanEqual","Property":"Height","Value":"1000"},{"Condition":"LessThanEqual","Property":"VideoBitrate","Value":"3000000"},`, 1)
	if q := transcoding(info("", width)); q.Get("MaxWidth") != "1280" || q.Get("MaxHeight") != "1000" || q.Get("VideoBitrate") != "3000000" {
		t.Fatalf("width-limited transcoding query %v", q)
	}
	// Coach encodes only to H.264 and only into HLS.
	for name, profile := range map[string]string{
		"codec":  strings.Replace(hlsProfile, `"h264,hevc"`, `"hevc"`, 1),
		"no hls": strings.Replace(hlsProfile, `"Protocol":"hls"`, `"Protocol":"http"`, 1),
	} {
		if source := info("", profile); source["ErrorCode"] != "NoCompatibleStream" || source["TranscodingUrl"] != nil {
			t.Fatalf("%s: %v", name, source)
		}
	}
	// HLS requests are refused for an item the catalog lacks, a codec list
	// without the source's codec or H.264, or a bad bitrate.
	expectStatus(t, request(h, "GET", "/Videos/missing/master.m3u8?PlaySessionId=a", "", "", token), 404)
	expectStatus(t, request(h, "GET", "/Videos/movie/master.m3u8?PlaySessionId=a&VideoCodec=hevc", "", "", token), 400)
	expectStatus(t, request(h, "GET", "/Videos/movie/master.m3u8?PlaySessionId=a&VideoBitrate=0", "", "", token), 400)
	expectStatus(t, request(h, "GET", "/Videos/movie/master.m3u8?PlaySessionId=a&VideoBitrate=1000000&MaxWidth=1", "", "", token), 400)
	w := request(h, "GET", "/Videos/movie/master.m3u8?PlaySessionId=t&VideoCodec=h264&VideoBitrate=1000000", "", "", token)
	expectStatus(t, w, 200)
	if body := w.Body.String(); !strings.HasPrefix(body, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1192000\n") {
		t.Fatalf("transcoding master playlist %q", body)
	}
	expectStatus(t, request(h, "GET", "/Videos/movie/master.m3u8?PlaySessionId=a", "", "", ""), 401)
	w = request(h, "GET", "/Videos/movie/master.m3u8?PlaySessionId=a&VideoCodec=h264", "", "", token)
	expectStatus(t, w, 200)
	if body := w.Body.String(); !strings.HasPrefix(body, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=") || !strings.Contains(body, "\nmain.m3u8?PlaySessionId=a&VideoCodec=h264\n") {
		t.Fatalf("master playlist %q", body)
	}
}

func TestStopActiveEncodings(t *testing.T) {
	store, _, dir := newTestServer(t)
	movie := playbackMovie()
	movie.Container = "mkv"
	server := New(store, "test", nil, &media.Catalog{ID: "movies", Items: []media.Item{movie}})
	m, err := hls.NewManager("ffmpeg", "ffprobe", filepath.Join(dir, "transcode"))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	server.SetHLS(m)
	h := server.Handler()
	token := login(t, h)
	// A media playlist request opens the session, even if the stream fails.
	busy := func(id string) bool {
		return request(h, "GET", "/Videos/movie/main.m3u8?VideoCodec=h264&PlaySessionId="+id, "", "", token).Code == 503
	}
	fill := func() {
		t.Helper()
		if busy("a") || busy("b") || !busy("c") {
			t.Fatalf("want %d open sessions and the next one refused", hls.MaxSessions)
		}
	}
	fill()
	expectStatus(t, request(h, "POST", "/Videos/ActiveEncodings/Delete?PlaySessionId=a", "", "", ""), 401)
	expectStatus(t, request(h, "POST", "/Videos/ActiveEncodings/Delete?PlaySessionId="+strings.Repeat("a", 129), "", "", token), 400)
	expectStatus(t, request(h, "POST", "/Videos/ActiveEncodings/Delete?DeviceId=d&PlaySessionId=a", "", "", token), 204)
	if busy("c") {
		t.Fatal("the stopped session still holds its slot")
	}
	// Without PlaySessionId every session of the caller ends.
	expectStatus(t, request(h, "DELETE", "/Videos/ActiveEncodings?DeviceId=d", "", "", token), 204)
	fill()
}

func TestPlaybackInfoRemuxesSecondaryAudio(t *testing.T) {
	store, _, dir := newTestServer(t)
	movie := playbackMovie()
	movie.Streams[1].Codec, movie.Streams[1].Channels = "aac", 2
	server := New(store, "test", nil, &media.Catalog{ID: "movies", Items: []media.Item{movie}})
	m, err := hls.NewManager("ffmpeg", "ffprobe", filepath.Join(dir, "transcode"))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	server.SetHLS(m)
	h := server.Handler()
	token := login(t, h)
	// Emby Web in Chromium cannot switch tracks in a file: its profile
	// accepts only the first audio track, as captured from 4.10.0.40.
	profile := strings.Replace(hlsProfile, `"CodecProfiles":[`, `"CodecProfiles":[{"Type":"VideoAudio","Codec":"aac",`+
		`"Conditions":[{"Condition":"Equals","Property":"IsSecondaryAudio","Value":"false","IsRequired":"false"}]},`, 1)
	info := func(query string) map[string]any {
		t.Helper()
		w := request(h, "POST", "/Items/movie/PlaybackInfo?"+query, "application/json", `{"DeviceProfile":`+profile+`}`, token)
		expectStatus(t, w, 200)
		var response struct{ MediaSources []map[string]any }
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response.MediaSources[0]
	}
	if source := info("AudioStreamIndex=1"); source["SupportsDirectStream"] != true || source["TranscodingUrl"] != nil {
		t.Fatalf("first track: %v", source)
	}
	source := info("AudioStreamIndex=2")
	raw, _ := source["TranscodingUrl"].(string)
	_, query, _ := strings.Cut(raw, "?")
	q, _ := url.ParseQuery(query)
	// The segments carry only that track, so it is copied, not encoded.
	if source["SupportsDirectStream"] != false || q.Get("AudioStreamIndex") != "2" || q.Get("AudioCodec") != "aac" || q.Has("MaxAudioChannels") {
		t.Fatalf("second track: %v", source)
	}
}
