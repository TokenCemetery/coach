package e2e

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestSoak runs mixed client load against one instance for COACH_SOAK (a Go
// duration such as "10m") and samples the process. It is skipped otherwise.
// Budgets are not agreed yet (#15), so it reports the numbers and fails only
// on leftovers: child processes, file descriptors or files in the data
// directory that outlive the load.
func TestSoak(t *testing.T) {
	duration, err := time.ParseDuration(os.Getenv("COACH_SOAK"))
	if err != nil {
		t.Skip("set COACH_SOAK to a duration, for example COACH_SOAK=10m")
	}
	workers := 8
	if n, err := strconv.Atoi(os.Getenv("COACH_SOAK_WORKERS")); err == nil && n > 0 {
		workers = n
	}
	inst, err := getEnv(t).suite.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Stop()
	pid := inst.cmd.Process.Pid
	baseline := sampleProcess(pid)
	dataFiles := listFiles(inst.data)

	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()
	var requests, failures atomic.Int64
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			for i := 0; ctx.Err() == nil; i++ {
				for _, r := range soakRequests(inst, w, i) {
					resp, err := inst.Send(r, true)
					requests.Add(1)
					if ctx.Err() == nil && (err != nil || resp.Status >= 300) {
						failures.Add(1)
						t.Logf("%s %s: status %d, err %v", r.Method, r.Path, resp.Status, err)
					}
				}
			}
		})
	}
	samples := []processSample{baseline}
	ticker := time.NewTicker(max(duration/20, time.Second))
	for done := false; !done; {
		select {
		case <-ctx.Done():
			done = true
		case <-ticker.C:
			samples = append(samples, sampleProcess(pid))
		}
	}
	ticker.Stop()
	wg.Wait()
	time.Sleep(3 * time.Second) // let finished FFmpeg jobs and connections close
	final := sampleProcess(pid)
	samples = append(samples, final)

	peak := slices.MaxFunc(samples, func(a, b processSample) int { return a.rssKiB - b.rssKiB })
	t.Logf("%d workers, %s: %d requests, %d failures", workers, duration, requests.Load(), failures.Load())
	t.Logf("RSS KiB: start %d, peak %d, end %d; open files: start %d, end %d", baseline.rssKiB, peak.rssKiB, final.rssKiB, baseline.files, final.files)
	for _, s := range samples {
		t.Logf("  %s rss=%dKiB files=%d children=%d", s.at.Format(time.TimeOnly), s.rssKiB, s.files, s.children)
	}
	if failures.Load() > 0 {
		t.Errorf("%d requests failed", failures.Load())
	}
	if final.children != 0 {
		t.Errorf("%d child processes remain after the load", final.children)
	}
	// A few descriptors may come and go (keep-alive pools, the log); a leak
	// grows with the number of requests.
	if final.files > baseline.files+8 {
		t.Errorf("open files grew from %d to %d", baseline.files, final.files)
	}
	if extra := listFiles(inst.data); !slices.Equal(extra, dataFiles) {
		t.Errorf("data directory changed from %v to %v", dataFiles, extra)
	}
}

// soakRequests is one iteration of a client browsing and watching: catalog,
// item, PlaybackInfo, the whole stream, a progress report, and
// every tenth iteration the embedded subtitle, which runs FFmpeg.
func soakRequests(inst *Instance, worker, iteration int) []Request {
	user := "/Users/" + inst.IDs.UserID
	stream := Request{Method: "GET", Path: "/Videos/" + inst.IDs.MovieID + "/stream.mp4",
		Query: url.Values{"Static": {"true"}, "MediaSourceId": {inst.IDs.MediaSourceID}, "DeviceId": {"soak-" + strconv.Itoa(worker)}}}
	progress := fmt.Sprintf(`{"ItemId":%q,"PositionTicks":%d}`, inst.IDs.MovieID, iteration*10000000)
	list := []Request{
		{Method: "GET", Path: user + "/Items", Query: url.Values{"Recursive": {"true"}, "IncludeItemTypes": {"Movie,Episode"}, "Limit": {"50"}}},
		{Method: "GET", Path: user + "/Items/" + inst.IDs.MovieID},
		{Method: "POST", Path: "/Items/" + inst.IDs.MovieID + "/PlaybackInfo", Body: []byte(`{}`), ContentType: "application/json"},
		stream,
		{Method: "POST", Path: "/Sessions/Playing/Progress", Body: []byte(progress), ContentType: "application/json"},
	}
	if iteration%10 == 0 && inst.IDs.SubtitleIndex != "" {
		list = append(list, Request{Method: "GET", Path: "/Videos/" + inst.IDs.MovieID + "/" + inst.IDs.MediaSourceID + "/Subtitles/" + inst.IDs.SubtitleIndex + "/Stream.vtt"})
	}
	return list
}

type processSample struct {
	at                      time.Time
	rssKiB, files, children int
}

func sampleProcess(pid int) processSample {
	s := processSample{at: time.Now()}
	p := strconv.Itoa(pid)
	if out, err := exec.CommandContext(context.Background(), "ps", "-o", "rss=", "-p", p).Output(); err == nil { //nolint:gosec // p is the PID of the instance under test
		s.rssKiB, _ = strconv.Atoi(strings.TrimSpace(string(out)))
	}
	if out, err := exec.CommandContext(context.Background(), "lsof", "-n", "-P", "-p", p).Output(); err == nil { //nolint:gosec // p is the PID of the instance under test
		s.files = max(strings.Count(string(out), "\n")-1, 0) // minus the header
	}
	// pgrep exits 1 when there are no children; the empty output is the answer.
	out, _ := exec.CommandContext(context.Background(), "pgrep", "-P", p).Output() //nolint:gosec // p is the PID of the instance under test
	s.children = len(strings.Fields(string(out)))
	return s
}

func listFiles(dir string) []string {
	names := []string{}
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			names = append(names, strings.TrimPrefix(path, dir))
		}
		return nil
	})
	return names
}
