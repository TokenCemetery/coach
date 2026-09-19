package server

import (
	"encoding/json"
	"math"
	"net/url"
	"strings"
	"testing"

	"github.com/TokenCemetery/coach/internal/media"
)

const compatibleProfile = `{"DirectPlayProfiles":[{"Type":"Video","Container":"mp4","VideoCodec":"h264","AudioCodec":"aac,ac3"}]}`

func playbackMovie() media.Item {
	return media.Item{ID: "movie", Container: "mp4", Size: 1000000, RunTimeTicks: 10000000, Streams: []media.Stream{
		{Index: 0, Type: "Video", Codec: "h264", Profile: "High", Level: 41, Width: 1920, Height: 1080, BitRate: 7000000, AverageFrameRate: 23.976, BitDepth: 8, CodecTag: "avc1"},
		{Index: 1, Type: "Audio", Codec: "ac3", Channels: 6, SampleRate: 48000},
		{Index: 2, Type: "Audio", Codec: "aac", Channels: 2, SampleRate: 48000, IsDefault: true},
		{Index: 3, Type: "Subtitle", Codec: "subrip"},
	}}
}

func TestPlaybackInfoNegotiation(t *testing.T) {
	s, _, _ := newTestServer(t)
	catalog := &media.Catalog{ID: "movies", Items: []media.Item{playbackMovie()}}
	h := New(s, "test", nil, catalog).Handler()
	token := login(t, h)
	for _, tc := range []struct {
		name, query, body string
		status            int
		direct            bool
	}{
		{"legacy discovery", "", "", 200, true},
		{"profile", "", `{"DeviceProfile":` + compatibleProfile + `}`, 200, true},
		{"empty profile", "", `{"DeviceProfile":{}}`, 200, false},
		{"no formats", "", `{"DeviceProfile":{"DirectPlayProfiles":[]}}`, 200, false},
		{"unsupported codec", "", `{"DeviceProfile":{"DirectPlayProfiles":[{"Type":"Video","VideoCodec":"hevc"}]}}`, 200, false},
		{"default not first audio", "", `{"DeviceProfile":{"DirectPlayProfiles":[{"Type":"Video","AudioCodec":"aac"}]}}`, 200, true},
		{"selected audio codec", "AudioStreamIndex=1", `{"DeviceProfile":{"DirectPlayProfiles":[{"Type":"Video","AudioCodec":"aac"}]}}`, 200, false},
		{"disabled query", "EnableDirectStream=false", "", 200, false},
		{"disabled body", "", `{"EnableDirectStream":false}`, 200, false},
		{"no transcoding fallback", "EnableDirectStream=false&EnableTranscoding=true", "", 200, false},
		{"bitrate query", "MaxStreamingBitrate=7999999", "", 200, false},
		{"bitrate body", "", `{"MaxStreamingBitrate":7999999}`, 200, false},
		{"bitrate exact", "MaxStreamingBitrate=8000000", "", 200, true},
		{"bitrate unlimited", "MaxStreamingBitrate=0", "", 200, true},
		{"profile bitrate", "MaxStreamingBitrate=9000000", `{"DeviceProfile":{"MaxStreamingBitrate":7000000,"DirectPlayProfiles":[{"Type":"Video"}]}}`, 200, false},
		{"max channels", "MaxAudioChannels=1", "", 200, false},
		{"default channels", "MaxAudioChannels=2", "", 200, true},
		{"selected channels", "AudioStreamIndex=1&MaxAudioChannels=2", "", 200, false},
		{"bad audio type", "AudioStreamIndex=0", "", 400, false},
		{"missing audio", "AudioStreamIndex=99", "", 400, false},
		{"subtitles off", "SubtitleStreamIndex=-1", "", 200, true},
		{"subtitles unsupported", "SubtitleStreamIndex=3", "", 200, false},
		{"missing subtitle", "SubtitleStreamIndex=99", "", 400, false},
		{"bad subtitle type", "SubtitleStreamIndex=1", "", 400, false},
		{"source", "MediaSourceId=mediasource_movie", "", 200, true},
		{"wrong source", "MediaSourceId=other", "", 404, false},
		{"wrong source body", "", `{"MediaSourceId":"other"}`, 404, false},
		{"foreign body user", "", `{"UserId":"other"}`, 403, false},
		{"foreign query user", "UserId=other", "", 403, false},
		{"same body user", "", `{"UserId":"` + s.Snapshot().User.ID + `"}`, 200, true},
		{"conflicting option", "EnableDirectStream=true", `{"EnableDirectStream":false}`, 400, false},
		{"conflicting duplicate", "MaxStreamingBitrate=1&maxstreamingbitrate=2", "", 400, false},
		{"equal duplicate", "MaxStreamingBitrate=8000000&maxstreamingbitrate=8000000", `{"MaxStreamingBitrate":8000000}`, 200, true},
		{"case insensitive query", "enabledirectstream=false", "", 200, false},
		{"negative bitrate", "MaxStreamingBitrate=-1", "", 400, false},
		{"invalid bitrate", "MaxStreamingBitrate=NaN", "", 400, false},
		{"fractional bitrate", "MaxStreamingBitrate=1.5", "", 400, false},
		{"invalid bool", "EnableDirectStream=perhaps", "", 400, false},
		{"invalid query encoding", "EnableDirectStream=%zz", "", 400, false},
		{"invalid query separator", "EnableDirectStream=false;ignored", "", 400, false},
		{"negative position", "StartTimeTicks=-1", "", 400, false},
		{"int32 overflow", "AudioStreamIndex=4294967298", "", 400, false},
		{"int64 overflow", "MaxStreamingBitrate=999999999999999999999", "", 400, false},
		{"malformed json", "", "{", 400, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := request(h, "POST", "/emby/Items/movie/PlaybackInfo?reqformat=json&"+tc.query, "text/plain", tc.body, token)
			expectStatus(t, w, tc.status)
			if tc.status != 200 {
				return
			}
			var response struct {
				ErrorCode, PlaySessionId string
				MediaSources             []struct {
					SupportsDirectPlay, SupportsDirectStream, SupportsTranscoding bool
					DirectStreamUrl                                               string
					DefaultAudioStreamIndex                                       int
				}
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if len(response.MediaSources) != 1 || response.PlaySessionId == "" {
				t.Fatal("invalid playback response")
			}
			source := response.MediaSources[0]
			if source.SupportsDirectStream != tc.direct || source.SupportsDirectPlay || source.SupportsTranscoding {
				t.Fatal("incorrect playback capability")
			}
			if !tc.direct {
				if source.DirectStreamUrl != "" || response.ErrorCode != "NoCompatibleStream" {
					t.Fatal("unsupported playback must not advertise a URL")
				}
			} else {
				u, err := url.Parse(source.DirectStreamUrl)
				if err != nil || u.IsAbs() || u.Path != "/videos/movie/stream.mp4" || u.Query().Get("api_key") != token || u.Query().Get("PlaySessionId") != response.PlaySessionId {
					t.Fatal("invalid authenticated stream URL")
				}
				if response.ErrorCode != "" || source.DefaultAudioStreamIndex != 2 {
					t.Fatal("incorrect default audio or error code")
				}
			}
		})
	}
	expectStatus(t, request(h, "GET", "/Items/movie/PlaybackInfo", "", "", ""), 401)
	expectStatus(t, request(h, "GET", "/Items/missing/PlaybackInfo", "", "", token), 404)
	w := request(h, "GET", "/Items/movie/PlaybackInfo?EnableDirectStream=false", "", "", token)
	expectStatus(t, w, 200)
	if !strings.Contains(w.Body.String(), `"ErrorCode":"NoCompatibleStream"`) {
		t.Fatal("GET ignored playback options")
	}
	w = request(h, "POST", "/Items/movie/PlaybackInfo?AudioStreamIndex=1", "application/json", `{"DeviceProfile":`+compatibleProfile+`}`, token)
	expectStatus(t, w, 200)
	if !strings.Contains(w.Body.String(), `"DefaultAudioStreamIndex":1`) || !strings.Contains(w.Body.String(), `"SupportsDirectStream":true`) {
		t.Fatal("selected audio track was not reflected in the source")
	}
}

func TestPlaybackProfileConditions(t *testing.T) {
	item := playbackMovie()
	video, audio := playbackStreams(item, nil)
	for _, tc := range []struct {
		name, extra string
		allowed     bool
	}{
		{"web h264", `"CodecProfiles":[{"Type":"Video","Codec":"h264","Conditions":[{"Condition":"EqualsAny","Property":"VideoProfile","Value":"high|main|baseline","IsRequired":false},{"Condition":"LessThanEqual","Property":"VideoLevel","Value":"42","IsRequired":false}]}]`, true},
		{"level limit", `"CodecProfiles":[{"Type":"Video","Conditions":[{"Condition":"LessThanEqual","Property":"VideoLevel","Value":"40"}]}]`, false},
		{"width limit", `"CodecProfiles":[{"Type":"Video","Conditions":[{"Condition":"LessThanEqual","Property":"Width","Value":"1280","IsRequired":false}]}]`, false},
		{"height limit", `"CodecProfiles":[{"Type":"Video","Conditions":[{"Condition":"LessThanEqual","Property":"Height","Value":720}]}]`, false},
		{"frame rate", `"CodecProfiles":[{"Type":"Video","Conditions":[{"Condition":"LessThanEqual","Property":"VideoFramerate","Value":"24"}]}]`, true},
		{"audio channels", `"CodecProfiles":[{"Type":"VideoAudio","Codec":"aac","Conditions":[{"Condition":"LessThanEqual","Property":"AudioChannels","Value":"1"}]}]`, false},
		{"web secondary audio", `"CodecProfiles":[{"Type":"VideoAudio","Conditions":[{"Condition":"Equals","Property":"IsSecondaryAudio","Value":"false","IsRequired":"false"}]}]`, false},
		{"sample rate alias", `"CodecProfiles":[{"Type":"VideoAudio","Conditions":[{"Condition":"LessThanEqual","Property":"SampleRate","Value":"48000"}]}]`, true},
		{"unrelated codec", `"CodecProfiles":[{"Type":"Video","Codec":"hevc","Conditions":[{"Condition":"LessThanEqual","Property":"Width","Value":"1280"}]}]`, true},
		{"unrelated container", `"CodecProfiles":[{"Type":"Video","Container":"avi","Conditions":[{"Condition":"LessThanEqual","Property":"Width","Value":"1280"}]}]`, true},
		{"unknown required", `"CodecProfiles":[{"Type":"Video","Conditions":[{"Condition":"Equals","Property":"VideoRotation","Value":0}]}]`, false},
		{"unknown optional", `"CodecProfiles":[{"Type":"Video","Conditions":[{"Condition":"Equals","Property":"VideoRotation","Value":0,"IsRequired":false}]}]`, true},
		{"apply false", `"CodecProfiles":[{"Type":"Video","ApplyConditions":[{"Condition":"Equals","Property":"VideoProfile","Value":"main"}],"Conditions":[{"Condition":"LessThanEqual","Property":"Width","Value":"1280"}]}]`, true},
		{"apply true", `"CodecProfiles":[{"Type":"Video","ApplyConditions":[{"Condition":"Equals","Property":"VideoProfile","Value":"High"}],"Conditions":[{"Condition":"LessThanEqual","Property":"Width","Value":"1280"}]}]`, false},
		{"container condition", `"ContainerProfiles":[{"Type":"Video","Container":"mp4","Conditions":[{"Condition":"LessThanEqual","Property":"NumAudioStreams","Value":"1"}]}]`, false},
		{"unknown operator", `"CodecProfiles":[{"Type":"Video","Conditions":[{"Condition":"Unknown","Property":"Width","Value":"1920"}]}]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var p deviceProfile
			if err := json.Unmarshal([]byte(strings.TrimSuffix(compatibleProfile, "}")+","+tc.extra+"}"), &p); err != nil {
				t.Fatal(err)
			}
			if got := supportsProfile(&p, item, video, audio); got != tc.allowed {
				t.Fatalf("compatible=%v, want %v", got, tc.allowed)
			}
		})
	}
	for _, tc := range []struct {
		op, value string
		actual    any
		want      bool
	}{
		{"GreaterThanEqual", "40", float64(41), true},
		{"EqualsAny", "8|10", float64(8), true},
		{"NotEquals", "xvid", "avc1", true},
		{"NotEquals", "HIGH", "High", false},
		{"LessThanEqual", "NaN", float64(1), false},
		{"NotEquals", "Infinity", float64(1), false},
		{"Equals", "garbage", false, false},
	} {
		if got := conditionMatches(profileCondition{Condition: tc.op, Value: conditionValue(tc.value)}, tc.actual); got != tc.want {
			t.Errorf("condition %s %s: %v", tc.op, tc.value, got)
		}
	}
}

func TestPlaybackBitrateBounds(t *testing.T) {
	if bitrate(media.Item{Size: math.MaxInt64, RunTimeTicks: 1}) != math.MaxInt64 {
		t.Fatal("bitrate overflow")
	}
	if bitrate(media.Item{Size: 1, RunTimeTicks: 30000000}) != 3 {
		t.Fatal("bitrate must round up")
	}
	item := playbackMovie()
	item.RunTimeTicks = 0
	video, audio := playbackStreams(item, nil)
	limit := int64(8000000)
	if (playbackRequest{MaxStreamingBitrate: &limit}).supportsDirectStream(item, video, audio) {
		t.Fatal("unknown bitrate must not pass an explicit limit")
	}
}
