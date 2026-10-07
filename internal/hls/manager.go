package hls

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	// MaxSessions caps concurrent remux sessions (one FFmpeg job each).
	MaxSessions = 2
	// maxSessionBytes caps the segments a session keeps on disk.
	maxSessionBytes = 2 << 30
	// A job pauses (SIGSTOP) when it has finished maxLead segments past the
	// last requested one and resumes at resumeLead; segments more than
	// keepBehind before it are deleted.
	maxLead    = 10
	resumeLead = 5
	keepBehind = 3
	// A request for a segment at most restartGap past the last written one
	// waits for the running job instead of restarting it.
	restartGap  = 3
	segmentWait = 30 * time.Second
	idleTimeout = 60 * time.Second
	pollEvery   = 100 * time.Millisecond
	maxCached   = 64
)

var (
	// ErrBusy means MaxSessions sessions are already open.
	ErrBusy = errors.New("too many HLS sessions")
	// ErrConflict means the session ID is in use for another source.
	ErrConflict = errors.New("HLS session belongs to another stream")
	// ErrNotFound means the segment is outside the playlist.
	ErrNotFound = errors.New("HLS segment not found")
	// ErrSuperseded means a newer request moved the job elsewhere; the client
	// may retry.
	ErrSuperseded = errors.New("HLS request superseded")
	// ErrSegment means FFmpeg could not produce the segment.
	ErrSegment = errors.New("HLS segment unavailable")
)

// Source is the stream a session remuxes.
type Source struct {
	ItemID string
	// Version changes when the file changes; it keys the keyframe cache.
	Version string
	Open    func() (*os.File, error)
	// Duration in seconds.
	Duration    float64
	VideoStream int
	// AudioStream is -1 for none. Audio is copied, or encoded to AAC with
	// AudioChannels channels when AudioEncode is set.
	AudioStream   int
	AudioEncode   bool
	AudioChannels int
}

// Key identifies what the session produces; a session ID cannot be reused
// for another key.
func (s Source) Key() string {
	return fmt.Sprintf("%s/%d/%d/%t/%d", s.ItemID, s.VideoStream, s.AudioStream, s.AudioEncode, s.AudioChannels)
}

// Manager owns HLS sessions and their FFmpeg jobs. Segments live in
// per-session directories under dir, which is emptied at start and on Close.
type Manager struct {
	ffmpeg, ffprobe, dir string

	mu       sync.Mutex
	sessions map[string]*session
	cache    map[string]*keyframeEntry
	closed   bool
	stop     chan struct{}
	reaped   chan struct{}
}

type keyframeEntry struct {
	done   chan struct{}
	bounds []float64
	start  float64 // the file's start time, which bounds are relative to
	err    error
}

type session struct {
	id, owner string
	source    Source
	dir       string

	mu         sync.Mutex
	job        *job
	lastAccess time.Time
	// lastRequest is read by the job's list reader, which must not take mu:
	// stopJob holds mu while it waits for the job to exit.
	lastRequest atomic.Int64
	// requests counts segment requests; only the newest may restart the job,
	// so a stale request that reaches the lock late cannot undo a seek.
	requests atomic.Int64
}

type job struct {
	start  int
	cmd    *exec.Cmd
	cancel context.CancelFunc
	// done closes after FFmpeg exits and its segment list is fully read; err
	// is set before.
	done chan struct{}
	err  error

	mu        sync.Mutex
	completed int // last segment FFmpeg finished, start-1 before the first
	paused    bool
}

// NewManager removes segments left by an earlier run and starts the idle
// session reaper.
func NewManager(ffmpeg, ffprobe, dir string) (*Manager, error) {
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	m := &Manager{ffmpeg: ffmpeg, ffprobe: ffprobe, dir: dir, sessions: map[string]*session{}, cache: map[string]*keyframeEntry{}, stop: make(chan struct{}), reaped: make(chan struct{})}
	go m.reap()
	return m, nil
}

// Close stops every job and removes all segments.
func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	sessions := m.sessions
	m.sessions = map[string]*session{}
	m.mu.Unlock()
	close(m.stop)
	<-m.reaped
	for _, s := range sessions {
		s.close()
	}
	_ = os.RemoveAll(m.dir)
}

func (m *Manager) reap() {
	defer close(m.reaped)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
		}
		m.mu.Lock()
		var idle []string
		for id, s := range m.sessions {
			s.mu.Lock()
			if time.Since(s.lastAccess) > idleTimeout {
				idle = append(idle, id)
			}
			s.mu.Unlock()
		}
		m.mu.Unlock()
		for _, id := range idle {
			m.remove(id)
		}
	}
}

// Available reports whether a new session could be opened now, or id is
// already open.
func (m *Manager) Available(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, open := m.sessions[id]
	return open || len(m.sessions) < MaxSessions
}

// Stop ends the session if owner opened it: its job is killed and its
// segments deleted.
func (m *Manager) Stop(id, owner string) {
	m.mu.Lock()
	s := m.sessions[id]
	m.mu.Unlock()
	if s != nil && s.owner == owner {
		m.remove(id)
	}
}

func (m *Manager) remove(id string) {
	m.mu.Lock()
	s := m.sessions[id]
	delete(m.sessions, id)
	m.mu.Unlock()
	if s != nil {
		s.close()
	}
}

// StopOwner ends every session opened by owner, for logout.
func (m *Manager) StopOwner(owner string) {
	m.mu.Lock()
	var ids []string
	for id, s := range m.sessions {
		if s.owner == owner {
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.remove(id)
	}
}

// Prepare lists the source's keyframes in the background, so that the
// playlist request that follows PlaybackInfo does not wait for the whole file
// to be read.
func (m *Manager) Prepare(source Source) {
	go func() { _, _, _ = m.bounds(context.Background(), source) }()
}

// bounds returns the cached segment plan and the file's start time, computing
// them once per file version. The computation is not tied to ctx, so an
// impatient client does not waste it.
func (m *Manager) bounds(ctx context.Context, source Source) ([]float64, float64, error) {
	key := source.ItemID + "/" + source.Version + "/" + strconv.Itoa(source.VideoStream)
	m.mu.Lock()
	entry, ok := m.cache[key]
	if !ok {
		if len(m.cache) >= maxCached {
			for k, e := range m.cache {
				select {
				case <-e.done:
					delete(m.cache, k)
				default:
				}
				if len(m.cache) < maxCached {
					break
				}
			}
		}
		entry = &keyframeEntry{done: make(chan struct{})}
		m.cache[key] = entry
		go func() { //nolint:gosec // deliberately outlives the request: the next one reuses the result
			defer close(entry.done)
			file, err := source.Open()
			if err != nil {
				entry.err = err
				return
			}
			defer func() { _ = file.Close() }()
			times, start, err := keyframes(context.Background(), m.ffprobe, file, source.VideoStream)
			if err != nil {
				slog.Warn("Keyframe listing failed", "item", source.ItemID)
				entry.err = err
				return
			}
			entry.bounds, entry.start = Boundaries(times), start
		}()
	}
	m.mu.Unlock()
	select {
	case <-entry.done:
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	}
	if entry.err != nil {
		// Let a later request try again, for example after a remount.
		m.mu.Lock()
		if m.cache[key] == entry {
			delete(m.cache, key)
		}
		m.mu.Unlock()
		return nil, 0, entry.err
	}
	return entry.bounds, entry.start, nil
}

// open returns the session id, creating it for source on first use.
func (m *Manager) open(id, owner string, source Source) (*session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrBusy
	}
	if s, ok := m.sessions[id]; ok {
		if s.owner != owner || s.source.Key() != source.Key() {
			return nil, ErrConflict
		}
		return s, nil
	}
	if len(m.sessions) >= MaxSessions {
		return nil, ErrBusy
	}
	dir, err := os.MkdirTemp(m.dir, "session-")
	if err != nil {
		return nil, err
	}
	s := &session{id: id, owner: owner, source: source, dir: dir, lastAccess: time.Now()}
	m.sessions[id] = s
	return s, nil
}

// Playlist returns the VOD media playlist of the session, opening it if
// needed.
func (m *Manager) Playlist(ctx context.Context, id, owner string, source Source, segmentURL func(n int) string) (string, error) {
	s, err := m.open(id, owner, source)
	if err != nil {
		return "", err
	}
	s.touch()
	bounds, _, err := m.bounds(ctx, source)
	if err != nil {
		return "", err
	}
	return Playlist(bounds, source.Duration, segmentURL), nil
}

// Segment returns segment n of the session, opening it if needed. It starts,
// restarts or waits for the FFmpeg job as required.
func (m *Manager) Segment(ctx context.Context, id, owner string, source Source, n int) (*os.File, error) {
	s, err := m.open(id, owner, source)
	if err != nil {
		return nil, err
	}
	request := s.requests.Add(1)
	bounds, start, err := m.bounds(ctx, source)
	if err != nil {
		return nil, err
	}
	if n < 0 || n >= len(bounds) {
		return nil, ErrNotFound
	}
	s.mu.Lock()
	if err := ctx.Err(); err != nil {
		// The client gave up while this request waited for the lock.
		s.mu.Unlock()
		return nil, err
	}
	s.lastAccess = time.Now()
	s.lastRequest.Store(int64(n))
	j := s.job
	if !s.ready(j, n) {
		// Restart on a seek outside the job's reach, after it exited, or for
		// a segment already pruned behind the playback position.
		if !s.reaches(j, n) {
			if s.requests.Load() != request {
				s.mu.Unlock()
				return nil, ErrSuperseded
			}
			s.stopJob()
			if err := s.startJob(m.ffmpeg, bounds, start, n); err != nil {
				s.mu.Unlock()
				slog.Warn("HLS job failed to start", "item", source.ItemID, "error", err)
				return nil, ErrSegment
			}
			j = s.job
		}
		j.resume()
	}
	s.mu.Unlock()
	timeout := time.NewTimer(segmentWait)
	defer timeout.Stop()
	for {
		s.mu.Lock()
		ready, current := s.ready(s.job, n), s.job
		s.mu.Unlock()
		if ready {
			return os.Open(s.segmentPath(n))
		}
		if current != j {
			// A newer request restarted the job; wait on the new one if it
			// will write this segment.
			s.mu.Lock()
			reaches := s.reaches(current, n)
			s.mu.Unlock()
			if !reaches {
				return nil, ErrSuperseded
			}
			j = current
			continue
		}
		if j.finished() {
			return nil, ErrSegment
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timeout.C:
			return nil, ErrSegment
		case <-time.After(pollEvery):
		}
	}
}

func (s *session) touch() {
	s.mu.Lock()
	s.lastAccess = time.Now()
	s.mu.Unlock()
}

func (s *session) segmentPath(n int) string {
	return filepath.Join(s.dir, fmt.Sprintf("seg-%05d.ts", n))
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// reaches reports whether job j has written segment n or soon will, so that
// a request for it can wait instead of restarting the job: n is not before
// the job's start, not pruned, and at most restartGap past the segment in
// progress. Caller holds s.mu.
func (s *session) reaches(j *job, n int) bool {
	if j == nil || n < j.start {
		return false
	}
	completed := j.completedSegment()
	if n <= completed {
		return exists(s.segmentPath(n))
	}
	return !j.finished() && n <= completed+1+restartGap
}

// ready reports whether FFmpeg has finished segment n in job j and it is
// still on disk. Caller holds s.mu.
func (s *session) ready(j *job, n int) bool {
	return j != nil && n >= j.start && n <= j.completedSegment() && exists(s.segmentPath(n))
}

func (j *job) completedSegment() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.completed
}

func (j *job) resume() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.paused && j.cmd.Process.Signal(syscall.SIGCONT) == nil {
		j.paused = false
	}
}

func (j *job) finished() bool {
	select {
	case <-j.done:
		return true
	default:
		return false
	}
}

// startJob writes segments from n on. Caller holds s.mu.
func (s *session) startJob(ffmpeg string, bounds []float64, start float64, n int) error {
	file, err := s.source.Open()
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	args := []string{"-v", "error", "-nostdin", "-protocol_whitelist", "fd"}
	if n > 0 {
		// Seek just before the segment's keyframe; -copypriorss 0 drops what
		// the demuxer returns before it, so the job starts on that keyframe.
		// FFmpeg compares that cutoff with absolute timestamps, so -ss is
		// absolute too (-seek_timestamp 1): with a relative one, a file that
		// starts below zero would lose the keyframe itself (#64).
		args = append(args, "-seek_timestamp", "1", "-ss", strconv.FormatFloat(start+bounds[n]-0.001, 'f', 6, 64))
	}
	args = append(args, "-fd", "3", "-i", "fd:", "-map", "0:"+strconv.Itoa(s.source.VideoStream))
	if s.source.AudioStream >= 0 {
		args = append(args, "-map", "0:"+strconv.Itoa(s.source.AudioStream))
	}
	args = append(args, "-c:v", "copy")
	if s.source.AudioEncode {
		args = append(args, "-c:a", "aac", "-ac", strconv.Itoa(s.source.AudioChannels))
	} else {
		args = append(args, "-c:a", "copy")
	}
	// Keep source timestamps so that segments of a restarted job line up with
	// those of the first. Segment times count from the job's first packet.
	args = append(args, "-copyts", "-avoid_negative_ts", "disabled")
	if n > 0 {
		args = append(args, "-copypriorss", "0")
	}
	args = append(args, "-f", "segment", "-segment_format", "mpegts", "-segment_start_number", strconv.Itoa(n))
	if n+1 < len(bounds) {
		times := make([]string, 0, len(bounds)-n-1)
		for _, b := range bounds[n+1:] {
			times = append(times, strconv.FormatFloat(b-bounds[n]-0.01, 'f', 3, 64))
		}
		args = append(args, "-segment_times", strings.Join(times, ","))
	}
	// FFmpeg names each segment on fd 4 as soon as it closes it.
	args = append(args, "-segment_list", "pipe:4", "-segment_list_type", "flat", filepath.Join(s.dir, "seg-%05d.ts"))
	list, listWriter, err := os.Pipe()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, ffmpeg, args...)
	cmd.Env = []string{"LC_ALL=C"}
	cmd.ExtraFiles = []*os.File{file, listWriter}
	cmd.WaitDelay = time.Second
	err = cmd.Start()
	_ = listWriter.Close()
	if err != nil {
		_ = list.Close()
		cancel()
		return err
	}
	j := &job{start: n, cmd: cmd, cancel: cancel, done: make(chan struct{}), completed: n - 1}
	s.job = j
	listed := make(chan struct{})
	go func() {
		defer close(listed)
		defer func() { _ = list.Close() }()
		s.follow(j, list)
	}()
	// j.err is written before done closes and read only after, so it needs
	// no lock; stopJob waits for done while holding s.mu.
	go func() {
		err := cmd.Wait()
		<-listed
		if err != nil && ctx.Err() == nil {
			slog.Warn("HLS job failed", "item", s.source.ItemID, "error", err)
		}
		if err != nil {
			j.err = err
		}
		close(j.done)
	}()
	go s.watch(j)
	return nil
}

// follow records each segment FFmpeg finishes and pauses the job (SIGSTOP)
// once it is maxLead segments ahead of the last request.
func (s *session) follow(j *job, list io.Reader) {
	lines := bufio.NewScanner(list)
	for lines.Scan() {
		name, _ := strings.CutPrefix(lines.Text(), "seg-")
		n, err := strconv.Atoi(strings.TrimSuffix(name, ".ts"))
		if err != nil {
			continue
		}
		j.mu.Lock()
		j.completed = n
		if !j.paused && int64(n)-s.lastRequest.Load() >= maxLead && j.cmd.Process.Signal(syscall.SIGSTOP) == nil {
			j.paused = true
		}
		j.mu.Unlock()
	}
}

// watch resumes the paused job as playback catches up, prunes old segments
// and enforces the disk limit until the job exits.
func (s *session) watch(j *job) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-j.done:
			return
		case <-ticker.C:
		}
		s.mu.Lock()
		if s.job != j {
			s.mu.Unlock()
			return
		}
		last := int(s.lastRequest.Load())
		if j.completedSegment()-last <= resumeLead {
			j.resume()
		}
		var size int64
		entries, _ := os.ReadDir(s.dir)
		for _, e := range entries {
			n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(e.Name(), "seg-"), ".ts"))
			if err == nil && n < last-keepBehind {
				_ = os.Remove(filepath.Join(s.dir, e.Name()))
				continue
			}
			if info, err := e.Info(); err == nil {
				size += info.Size()
			}
		}
		if size > maxSessionBytes {
			slog.Warn("HLS session exceeded its disk limit", "item", s.source.ItemID)
			j.cancel()
		}
		s.mu.Unlock()
	}
}

// stopJob kills the job and deletes its segments. Caller holds s.mu.
func (s *session) stopJob() {
	j := s.job
	if j == nil {
		return
	}
	s.job = nil
	j.cancel()
	<-j.done
	entries, _ := os.ReadDir(s.dir)
	for _, e := range entries {
		_ = os.Remove(filepath.Join(s.dir, e.Name()))
	}
}

func (s *session) close() {
	s.mu.Lock()
	s.stopJob()
	s.mu.Unlock()
	_ = os.RemoveAll(s.dir)
}
