package e2e

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// TestSoak runs mixed client load against one instance for COACH_SOAK (a Go
// duration such as "10m") and samples the process. It is skipped otherwise.
// Workers 0 and 1 also remux to HLS and worker 2 transcodes, which fills the
// FFmpeg session limits, and the media directory is rescanned (SIGHUP)
// during the load.
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
				// Every fifth round reconnects the WebSocket, alternately
				// closing it cleanly and dropping the connection.
				if i%5 == 0 {
					err := reconnectSocket(inst, i%10 == 0)
					requests.Add(1)
					if err != nil && ctx.Err() == nil {
						failures.Add(1)
						t.Logf("websocket: %v", err)
					}
				}
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
	// Rescan during playback, as -rescan-interval or an admin's SIGHUP would.
	var rescans atomic.Int64
	wg.Go(func() {
		tick := time.NewTicker(max(duration/8, 5*time.Second))
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if err := inst.cmd.Process.Signal(syscall.SIGHUP); err == nil {
					rescans.Add(1)
				}
			}
		}
	})
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
	t.Logf("%d workers, %s: %d requests, %d failures, %d rescans", workers, duration, requests.Load(), failures.Load(), rescans.Load())
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

// reconnectSocket opens /embywebsocket like Emby Web, waits for the server's
// first message and then closes with a close frame or by dropping the TCP
// connection, which is what a closed browser tab does.
func reconnectSocket(inst *Instance, clean bool) error {
	address := strings.TrimPrefix(inst.URL, "http://")
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(context.Background(), "tcp", address)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	key := make([]byte, 16)
	_, _ = rand.Read(key)
	query := url.Values{"api_key": {inst.Token}, "deviceId": {"soak-socket"}}
	request := "GET /embywebsocket?" + query.Encode() + " HTTP/1.1\r\nHost: " + address +
		"\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " +
		base64.StdEncoding.EncodeToString(key) + "\r\n\r\n"
	if _, err := conn.Write([]byte(request)); err != nil {
		return err
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(status, " 101 ") {
		return fmt.Errorf("handshake: %q, %w", strings.TrimSpace(status), err)
	}
	for line := ""; line != "\r\n"; {
		if line, err = reader.ReadString('\n'); err != nil {
			return err
		}
	}
	// The first frame is ForceKeepAlive; reading its header proves the socket
	// is served, not just upgraded.
	if _, err := reader.Peek(2); err != nil {
		return fmt.Errorf("first frame: %w", err)
	}
	if clean {
		// A masked close frame without payload, as RFC 6455 requires of clients.
		_, err = conn.Write([]byte{0x88, 0x80, 0, 0, 0, 0})
	}
	return err
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
	if worker < 3 && iteration%4 == 0 {
		list = append(list, hlsSession(inst, worker, iteration)...)
	}
	return list
}

// hlsSession plays the first segment of an HLS stream and ends the session
// as Emby Web does on stop. Workers 0 and 1 remux, worker 2 transcodes, so
// together they fill both FFmpeg limits without exceeding them.
func hlsSession(inst *Instance, worker, iteration int) []Request {
	query := url.Values{"MediaSourceId": {inst.IDs.MediaSourceID}, "VideoCodec": {"h264"}, "AudioCodec": {"aac"},
		"PlaySessionId": {fmt.Sprintf("soak-hls-%d-%d", worker, iteration)}, "DeviceId": {"soak-" + strconv.Itoa(worker)}}
	if worker == 2 {
		query.Set("VideoBitrate", "500000")
	}
	base := "/Videos/" + inst.IDs.MovieID
	return []Request{
		{Method: "GET", Path: base + "/master.m3u8", Query: query},
		{Method: "GET", Path: base + "/main.m3u8", Query: query},
		{Method: "GET", Path: base + "/hls1/main/0.ts", Query: query},
		{Method: "POST", Path: "/Videos/ActiveEncodings/Delete", Query: url.Values{"PlaySessionId": query["PlaySessionId"]}},
	}
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
