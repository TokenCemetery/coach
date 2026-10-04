package e2e

import (
	"bytes"
	"context"
	"mime"
	"net/url"
	"os"
	"os/exec"
	"path"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ozontech/allure-go/pkg/framework/provider"
	"github.com/ozontech/allure-go/pkg/framework/runner"
)

// behavior is a scenario check where a contract-shaped response is not
// enough evidence. Each runs on its own instance.
type behavior struct {
	Key     string
	Title   string
	Feature string
	Run     func(inst *Instance, check *Check)
}

var behaviors = []behavior{
	{
		Key:     "hls-master-playlist",
		Title:   "HLS master playlist of a movie is playable by FFprobe",
		Feature: "DynamicHlsService",
		Run: func(inst *Instance, check *Check) {
			q := url.Values{"MediaSourceId": {inst.IDs.MediaSourceID}, "PlaySessionId": {"e2e"},
				"VideoCodec": {"h264"}, "AudioCodec": {"aac"}, "api_key": {inst.Token}}
			r := Request{Method: "GET", Path: "/Videos/" + inst.IDs.MovieID + "/master.m3u8", Query: q}
			resp, err := inst.Send(r, true)
			if err != nil {
				check.Errorf("master.m3u8: %v", err)
				return
			}
			if resp.Status != 200 || !bytes.HasPrefix(resp.Body, []byte("#EXTM3U")) {
				check.Errorf("master.m3u8: status %d, body %q, want 200 with #EXTM3U", resp.Status, snippet(resp.Body))
				return
			}
			probe := exec.CommandContext(context.Background(), "ffprobe", "-v", "error", //nolint:gosec // URL of the local test instance
				"-show_entries", "stream=codec_type", "-of", "csv=p=0", r.URL(inst.URL))
			out, err := probe.CombinedOutput()
			if err != nil || !strings.Contains(string(out), "video") {
				check.Errorf("ffprobe cannot read the HLS stream: %v: %s", err, out)
			}
		},
	},
	{
		Key:     "subtitle-webvtt",
		Title:   "Subtitle stream of a movie is delivered as WebVTT",
		Feature: "SubtitleService",
		Run: func(inst *Instance, check *Check) {
			if inst.IDs.SubtitleIndex == "" {
				check.Errorf("the movie lists no subtitle stream in MediaStreams")
				return
			}
			r := Request{Method: "GET", Path: "/Videos/" + inst.IDs.MovieID + "/" + inst.IDs.MediaSourceID +
				"/Subtitles/" + inst.IDs.SubtitleIndex + "/Stream.vtt"}
			resp, err := inst.Send(r, true)
			if err != nil {
				check.Errorf("subtitle: %v", err)
				return
			}
			if resp.Status != 200 || !bytes.HasPrefix(resp.Body, []byte("WEBVTT")) || !bytes.Contains(resp.Body, []byte("Coach e2e subtitle")) {
				check.Errorf("subtitle: status %d, body %q, want 200 WebVTT with the cue text", resp.Status, snippet(resp.Body))
			}
		},
	},
	{
		Key:     "download-attachment",
		Title:   "Download serves the movie file as an attachment to a token in the URL",
		Feature: "LibraryService",
		Run: func(inst *Instance, check *Check) {
			// Emby Web navigates to the URL, so only api_key authorizes it.
			q := url.Values{"mediaSourceId": {inst.IDs.MediaSourceID}, "api_key": {inst.Token}}
			resp, err := inst.Send(Request{Method: "GET", Path: "/Items/" + inst.IDs.MovieID + "/Download", Query: q}, false)
			if err != nil {
				check.Errorf("download: %v", err)
				return
			}
			_, params, _ := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
			if resp.Status != 200 || len(resp.Body) == 0 || resp.Header.Get("Content-Length") != strconv.Itoa(len(resp.Body)) ||
				!strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") || path.Ext(params["filename"]) == "" {
				check.Errorf("download: status %d, %d bytes, Content-Disposition %q, want 200 attachment with the whole file",
					resp.Status, len(resp.Body), resp.Header.Get("Content-Disposition"))
			}
		},
	},
	{
		Key:     "resume-includes-next-up",
		Title:   "Continue Watching with IncludeNextUp offers the next episode",
		Feature: "UserLibraryService",
		Run: func(inst *Instance, check *Check) {
			var episodes struct {
				Items []struct {
					Id                string
					IndexNumber       int
					ParentIndexNumber int
				}
			}
			if err := inst.Get("/emby/Shows/"+inst.IDs.SeriesID+"/Episodes?UserId="+inst.IDs.UserID, &episodes); err != nil {
				check.Errorf("episodes: %v", err)
				return
			}
			var first, second string
			for _, ep := range episodes.Items {
				if ep.ParentIndexNumber == 1 && ep.IndexNumber == 1 {
					first = ep.Id
				}
				if ep.ParentIndexNumber == 1 && ep.IndexNumber == 2 {
					second = ep.Id
				}
			}
			if first == "" || second == "" {
				check.Errorf("S01E01 and S01E02 not found among %d episodes", len(episodes.Items))
				return
			}
			played := Request{Method: "POST", Path: "/Users/" + inst.IDs.UserID + "/PlayedItems/" + first}
			if resp, err := inst.Send(played, true); err != nil || resp.Status/100 != 2 {
				check.Errorf("mark S01E01 played: status %d, err %v", resp.Status, err)
				return
			}
			var resume struct{ Items []struct{ Id string } }
			if err := inst.Get("/emby/Users/"+inst.IDs.UserID+"/Items/Resume?IncludeNextUp=true&MediaTypes=Video", &resume); err != nil {
				check.Errorf("resume: %v", err)
				return
			}
			ids := make([]string, 0, len(resume.Items))
			for _, it := range resume.Items {
				ids = append(ids, it.Id)
			}
			if !slices.Contains(ids, second) {
				check.Errorf("resume items %v do not include S01E02 %s", ids, second)
			}
		},
	},
	{
		Key:     "forced-stop-keeps-confirmed-state",
		Title:   "SIGKILL during playback reports keeps every confirmed position",
		Feature: "PlaystateService",
		Run: func(inst *Instance, check *Check) {
			report := func(path string, position int) (int, error) {
				body := `{"ItemId":"` + inst.IDs.MovieID + `","PositionTicks":` + strconv.Itoa(position) + `}`
				resp, err := inst.Send(Request{Method: "POST", Path: path, Body: []byte(body), ContentType: "application/json"}, true)
				return resp.Status, err
			}
			if status, err := report("/Sessions/Playing", 0); err != nil || status != 204 {
				check.Errorf("start: status %d, err %v", status, err)
				return
			}
			// Reports arrive back to back; the kill lands while one is being
			// written. Positions only grow, so the stored one must be at least
			// the last confirmed and at most the last sent.
			confirmed, sent := 0, 0
			done := make(chan struct{})
			go func() {
				defer close(done)
				for position := 1; ; position++ {
					sent = position
					if status, err := report("/Sessions/Playing/Progress", position); err != nil || status != 204 {
						return
					}
					confirmed = position
				}
			}()
			time.Sleep(300 * time.Millisecond)
			if err := inst.KillAndRestart(); err != nil {
				check.Errorf("restart after SIGKILL: %v", err)
				return
			}
			<-done
			var item struct {
				UserData struct{ PlaybackPositionTicks int }
			}
			if err := inst.Get("/emby/Users/"+inst.IDs.UserID+"/Items/"+inst.IDs.MovieID, &item); err != nil {
				check.Errorf("item after restart: %v", err)
				return
			}
			if got := item.UserData.PlaybackPositionTicks; confirmed == 0 || got < confirmed || got > sent {
				check.Errorf("position after restart %d, want between last confirmed %d and last sent %d", got, confirmed, sent)
			}
		},
	},
	{
		Key:     "storage-error-is-reported",
		Title:   "A failed state write is reported and not published",
		Feature: "UserLibraryService",
		Run: func(inst *Instance, check *Check) {
			favorite := Request{Method: "POST", Path: "/Users/" + inst.IDs.UserID + "/FavoriteItems/" + inst.IDs.MovieID}
			isFavorite := func() (bool, error) {
				var item struct{ UserData struct{ IsFavorite bool } }
				err := inst.Get("/emby/Users/"+inst.IDs.UserID+"/Items/"+inst.IDs.MovieID, &item)
				return item.UserData.IsFavorite, err
			}
			// A read-only data directory fails the temp file creation, like a
			// full disk or a read-only remount.
			if os.Geteuid() == 0 {
				check.Errorf("run the suite as a regular user: root ignores directory permissions")
				return
			}
			if err := os.Chmod(inst.data, 0o500); err != nil { //nolint:gosec // a directory needs its execute bit
				check.Errorf("make data read-only: %v", err)
				return
			}
			resp, err := inst.Send(favorite, true)
			if err := os.Chmod(inst.data, 0o700); err != nil { //nolint:gosec // the mode Coach creates the data directory with
				check.Errorf("restore data permissions: %v", err)
				return
			}
			if err != nil || resp.Status != 500 {
				check.Errorf("favorite on read-only storage: status %d, err %v, want 500", resp.Status, err)
			}
			if got, err := isFavorite(); err != nil || got {
				check.Errorf("after the failed write: IsFavorite %v, err %v, want false", got, err)
			}
			if resp, err := inst.Send(favorite, true); err != nil || resp.Status != 200 {
				check.Errorf("favorite after storage recovers: status %d, err %v, want 200", resp.Status, err)
			}
			if got, err := isFavorite(); err != nil || !got {
				check.Errorf("after recovery: IsFavorite %v, err %v, want true", got, err)
			}
		},
	},
}

// TestBehavior runs the scenario checks.
func TestBehavior(t *testing.T) {
	e := getEnv(t)
	for _, b := range behaviors {
		runner.Run(t, b.Key, func(pt provider.T) {
			pt.Parallel()
			pt.Title(b.Title)
			pt.Epic("Behavior")
			pt.Feature(b.Feature)
			pt.Story(b.Key)
			inst, err := e.suite.Start()
			if err != nil {
				pt.Fatalf("start instance: %v", err)
			}
			defer inst.Stop()
			check := &Check{}
			b.Run(inst, check)
			Settle(pt, b.Key, check)
		})
	}
}
