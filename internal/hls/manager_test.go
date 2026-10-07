package hls

import (
	"context"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestBoundariesAndPlaylist(t *testing.T) {
	bounds := Boundaries([]float64{0, 2.6, 5.2, 7.84, 10.4, 13, 15.64, 20})
	if want := []float64{0, 7.84, 15.64}; !equal(bounds, want) {
		t.Fatalf("bounds %v, want %v", bounds, want)
	}
	got := Playlist(bounds, 21, func(n int) string { return "s/" + strconv.Itoa(n) + ".ts" })
	want := "#EXTM3U\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:8\n#EXT-X-MEDIA-SEQUENCE:0\n" +
		"#EXTINF:7.840000,\ns/0.ts\n#EXTINF:7.800000,\ns/1.ts\n#EXTINF:5.360000,\ns/2.ts\n#EXT-X-ENDLIST\n"
	if got != want {
		t.Fatalf("playlist:\n%s", got)
	}
}

func equal(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if math.Abs(a[i]-b[i]) > 1e-9 {
			return false
		}
	}
	return true
}

// fixture builds a 40-second video with keyframes every 2.6 seconds and AC3
// audio that starts half a second before the video, in the given container.
func fixture(t *testing.T, name string) string {
	return fixtureOf(t, name, 40)
}

func fixtureOf(t *testing.T, name string, seconds int) string {
	t.Helper()
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " required")
		}
	}
	path := filepath.Join(t.TempDir(), name)
	build := exec.CommandContext(context.Background(), "ffmpeg", "-v", "error", "-nostdin", //nolint:gosec // fixed arguments and temp paths
		"-itsoffset", "0.5", "-f", "lavfi", "-i", "testsrc2=s=160x120:d="+strconv.Itoa(seconds)+":r=25", "-f", "lavfi", "-i", "sine=d="+strconv.Itoa(seconds+1),
		"-c:v", "libx264", "-g", "1000", "-force_key_frames", "expr:gte(t,n_forced*2.6)", "-c:a", "ac3", path)
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("cannot build fixture: %v %s", err, out)
	}
	return path
}

// firstVideoPTS is the presentation time of the first video packet.
func firstVideoPTS(t *testing.T, file *os.File) float64 {
	t.Helper()
	defer func() { _ = file.Close() }()
	out, err := exec.CommandContext(context.Background(), "ffprobe", "-v", "error", "-select_streams", "v:0", //nolint:gosec // test segment path
		"-show_entries", "packet=pts_time", "-of", "csv=p=0", "-read_intervals", "%+#1", file.Name()).Output()
	if err != nil {
		t.Fatal(err)
	}
	v, err := strconv.ParseFloat(strings.Trim(strings.Split(string(out), "\n")[0], " ,"), 64)
	if err != nil {
		t.Fatalf("pts %q: %v", out, err)
	}
	return v
}

func TestRemuxSegmentsMatchThePlan(t *testing.T) {
	for _, name := range []string{"in.mkv", "in.ts"} {
		t.Run(name, func(t *testing.T) {
			path := fixture(t, name)
			dir := filepath.Join(t.TempDir(), "transcode")
			if err := os.MkdirAll(filepath.Join(dir, "stale"), 0700); err != nil {
				t.Fatal(err)
			}
			m, err := NewManager("ffmpeg", "ffprobe", dir)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			if exists(filepath.Join(dir, "stale")) {
				t.Fatal("leftover segments survived the start")
			}
			source := Source{ItemID: "movie", Version: "1", Duration: 40, VideoStream: 0, AudioStream: 1, AudioEncode: true, AudioChannels: 2,
				Open: func() (*os.File, error) { return os.Open(path) }}
			ctx := context.Background()
			playlist, err := m.Playlist(ctx, "a", "owner", source, func(n int) string { return strconv.Itoa(n) + ".ts" })
			if err != nil {
				t.Fatal(err)
			}
			bounds, _, _ := m.bounds(ctx, source)
			if len(bounds) < 5 || strings.Count(playlist, "#EXTINF") != len(bounds) {
				t.Fatalf("bounds %v, playlist:\n%s", bounds, playlist)
			}
			// From the start: each segment begins on its planned keyframe.
			// Segment 0 may start up to one AAC frame late: encoder priming
			// shifts the first segment when audio starts before video.
			sequential := make([]float64, len(bounds))
			for n := range bounds {
				file, err := m.Segment(ctx, "a", "owner", source, n)
				if err != nil {
					t.Fatalf("segment %d: %v", n, err)
				}
				sequential[n] = firstVideoPTS(t, file)
			}
			for n := range bounds {
				tolerance := 0.002
				if n == 0 {
					tolerance = 0.05
				}
				if got, want := sequential[n]-sequential[1], bounds[n]-bounds[1]; math.Abs(got-want) > tolerance {
					t.Fatalf("segment %d starts at %+.3f from segment 1, plan %+.3f", n, got, want)
				}
			}
			// After a seek, a restarted job writes the same segments.
			for _, n := range []int{3, 1} {
				file, err := m.Segment(ctx, "b", "owner", source, n)
				if err != nil {
					t.Fatalf("restart at %d: %v", n, err)
				}
				if got := firstVideoPTS(t, file); math.Abs(got-sequential[n]) > 0.002 {
					t.Fatalf("restarted segment %d starts at %.3f, sequential %.3f", n, got, sequential[n])
				}
			}
			if _, err := m.Segment(ctx, "a", "owner", source, len(bounds)); !errors.Is(err, ErrNotFound) {
				t.Fatalf("segment past the end: %v", err)
			}
			if _, err := m.Segment(ctx, "a", "other", source, 0); !errors.Is(err, ErrConflict) {
				t.Fatalf("foreign owner: %v", err)
			}
			if _, err := m.Playlist(ctx, "c", "owner", source, strconv.Itoa); !errors.Is(err, ErrBusy) || m.Available("c") {
				t.Fatalf("third session: %v", err)
			}
			m.Stop("a", "other")
			if m.Available("c") {
				t.Fatal("a foreign owner stopped a session")
			}
			m.Stop("a", "owner")
			if !m.Available("c") {
				t.Fatal("session a not stopped")
			}
			m.StopOwner("owner")
			if entries, _ := os.ReadDir(dir); len(entries) != 0 || !m.Available("c") {
				t.Fatalf("stopped sessions left %d entries", len(entries))
			}
		})
	}
}

// A job far ahead of the player pauses instead of writing the whole video.
func TestRemuxJobPausesAhead(t *testing.T) {
	path := fixtureOf(t, "long.mkv", 180)
	m, err := NewManager("ffmpeg", "ffprobe", filepath.Join(t.TempDir(), "transcode"))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	source := Source{ItemID: "long", Version: "1", Duration: 180, VideoStream: 0, AudioStream: 1,
		Open: func() (*os.File, error) { return os.Open(path) }}
	ctx := context.Background()
	bounds, _, err := m.bounds(ctx, source)
	if err != nil || len(bounds) <= maxLead+restartGap+2 {
		t.Fatalf("bounds %d: %v", len(bounds), err)
	}
	file, err := m.Segment(ctx, "a", "owner", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	m.mu.Lock()
	s := m.sessions["a"]
	m.mu.Unlock()
	s.mu.Lock()
	j := s.job
	s.mu.Unlock()
	deadline := time.Now().Add(10 * time.Second)
	for {
		j.mu.Lock()
		paused, completed := j.paused, j.completed
		j.mu.Unlock()
		if paused {
			// SIGSTOP is not instant: FFmpeg may close one more segment.
			if completed < maxLead || completed > maxLead+1 {
				t.Fatalf("paused after segment %d, want %d", completed, maxLead)
			}
			break
		}
		if time.Now().After(deadline) || j.finished() {
			t.Fatalf("job did not pause; finished %d of %d segments", completed, len(bounds))
		}
		time.Sleep(20 * time.Millisecond)
	}
	// A request within reach resumes the paused job instead of restarting it.
	file, err = m.Segment(ctx, "a", "owner", source, maxLead+1)
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job != j {
		t.Fatal("the job restarted instead of resuming")
	}
}

// A stale request that reaches the session lock after a newer one arrived, as
// when the player aborts a request and seeks, must not move the job.
func TestStaleRequestDoesNotMoveTheJob(t *testing.T) {
	path := fixtureOf(t, "long.mkv", 180)
	m, err := NewManager("ffmpeg", "ffprobe", filepath.Join(t.TempDir(), "transcode"))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	source := Source{ItemID: "long", Version: "1", Duration: 180, VideoStream: 0, AudioStream: 1,
		Open: func() (*os.File, error) { return os.Open(path) }}
	ctx := context.Background()
	file, err := m.Segment(ctx, "a", "owner", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	m.mu.Lock()
	s := m.sessions["a"]
	m.mu.Unlock()
	// The request for 21 takes its number and waits for the lock; then a
	// newer request arrives.
	s.mu.Lock()
	result := make(chan error, 1)
	go func() {
		file, err := m.Segment(ctx, "a", "owner", source, 21)
		if file != nil {
			_ = file.Close()
		}
		result <- err
	}()
	for s.requests.Load() < 2 {
		time.Sleep(time.Millisecond)
	}
	s.requests.Add(1)
	s.mu.Unlock()
	if err := <-result; !errors.Is(err, ErrSuperseded) {
		t.Fatalf("stale request: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job.start != 0 {
		t.Fatalf("a stale request moved the job to %d", s.job.start)
	}
}
