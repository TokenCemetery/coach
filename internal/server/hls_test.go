package server

import (
	"encoding/json"
	"net/url"
	"path/filepath"
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
	// Video the profile rejects needs transcoding, which is not implemented.
	for name, profile := range map[string]string{
		"codec":   strings.Replace(hlsProfile, `"h264,hevc"`, `"hevc"`, 1),
		"level":   strings.Replace(hlsProfile, `"Value":"51"`, `"Value":"40"`, 1),
		"no hls":  strings.Replace(hlsProfile, `"Protocol":"hls"`, `"Protocol":"http"`, 1),
		"bitrate": strings.Replace(hlsProfile, `{"DirectPlayProfiles"`, `{"MaxStreamingBitrate":1000,"DirectPlayProfiles"`, 1),
	} {
		if source := info("", profile); source["ErrorCode"] != "NoCompatibleStream" || source["TranscodingUrl"] != nil {
			t.Fatalf("%s: %v", name, source)
		}
	}
	// HLS requests are refused for an item the catalog lacks or a codec the
	// remux cannot copy.
	expectStatus(t, request(h, "GET", "/Videos/missing/master.m3u8?PlaySessionId=a", "", "", token), 404)
	expectStatus(t, request(h, "GET", "/Videos/movie/master.m3u8?PlaySessionId=a&VideoCodec=hevc", "", "", token), 400)
	expectStatus(t, request(h, "GET", "/Videos/movie/master.m3u8?PlaySessionId=a", "", "", ""), 401)
	w := request(h, "GET", "/Videos/movie/master.m3u8?PlaySessionId=a&VideoCodec=h264", "", "", token)
	expectStatus(t, w, 200)
	if body := w.Body.String(); !strings.HasPrefix(body, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=") || !strings.Contains(body, "\nmain.m3u8?PlaySessionId=a&VideoCodec=h264\n") {
		t.Fatalf("master playlist %q", body)
	}
}
