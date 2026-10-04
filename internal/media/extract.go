package media

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"time"
)

const (
	maxSubtitleJobs   = 2
	subtitleJobLimit  = 2 * time.Minute
	errSubtitleOutput = "subtitle output limit exceeded"
)

// subtitleExtractor runs FFmpeg to convert one embedded subtitle track to
// WebVTT. At most maxSubtitleJobs run at once; further requests wait for a
// slot until their own context ends. Output goes through a pipe, so a killed
// job leaves no temp files.
type subtitleExtractor struct {
	binary string
	slots  chan struct{}
}

func newSubtitleExtractor(binary string) *subtitleExtractor {
	return &subtitleExtractor{binary: binary, slots: make(chan struct{}, maxSubtitleJobs)}
}

type limitedOutput struct {
	buffer bytes.Buffer
	cancel context.CancelFunc
}

func (b *limitedOutput) Write(p []byte) (int, error) {
	if b.buffer.Len()+len(p) > maxSubtitleBytes {
		b.cancel()
		return 0, errors.New(errSubtitleOutput)
	}
	return b.buffer.Write(p)
}

// extract reads the whole file: subtitle packets are spread through it. The
// job is killed when ctx ends (client disconnect) or after subtitleJobLimit.
func (e *subtitleExtractor) extract(ctx context.Context, file *os.File, index int) ([]byte, error) {
	select {
	case e.slots <- struct{}{}:
		defer func() { <-e.slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, subtitleJobLimit)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.binary, "-v", "error", "-nostdin", "-threads", "1", //nolint:gosec // binary is ffmpeg from PATH; the only variable argument is an integer
		"-protocol_whitelist", "fd", "-fd", "3", "-i", "fd:",
		"-map", "0:"+strconv.Itoa(index), "-c:s", "webvtt", "-f", "webvtt", "pipe:1")
	// Do not inherit FFREPORT, credentials, or other application environment variables.
	cmd.Env = []string{"LC_ALL=C"}
	cmd.ExtraFiles = []*os.File{file}
	output := &limitedOutput{cancel: cancel}
	cmd.Stdout = output
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		return nil, errors.New("ffmpeg failed or exceeded its limits")
	}
	return output.buffer.Bytes(), nil
}
