package media

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseProbe(t *testing.T) {
	item, err := parseProbe([]byte(`{"format":{"duration":"1.25"},"streams":[
		{"index":0,"codec_type":"video","codec_name":"h264","width":1920,"height":1080,"disposition":{"default":1}},
		{"index":1,"codec_type":"audio","codec_name":"aac","channels":2,"sample_rate":"48000","bit_rate":"128000","tags":{"language":"rus"}},
		{"index":2,"codec_type":"subtitle","codec_name":"subrip","disposition":{"forced":1}},
		{"index":3,"codec_type":"video","codec_name":"mjpeg","disposition":{"attached_pic":1}},
		{"index":4,"codec_type":"data"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if item.RunTimeTicks != 12500000 || len(item.Streams) != 3 {
		t.Fatal("incorrect duration/stream count")
	}
	if item.Streams[0].Width != 1920 || !item.Streams[0].IsDefault || item.Streams[1].Language != "rus" || item.Streams[1].SampleRate != 48000 || item.Streams[1].BitRate != 128000 || !item.Streams[2].IsForced {
		t.Fatal("incorrect stream metadata")
	}
	for _, raw := range []string{"{", "null", `{}`, `{"streams":[{"codec_type":"audio"}]}`, `{"streams":[{"codec_type":"video","disposition":{"attached_pic":1}}]}`} {
		if _, err := parseProbe([]byte(raw)); err == nil {
			t.Fatal("invalid/nonvideo probe accepted")
		}
	}
	for _, duration := range []string{"N/A", "NaN", "+Inf", "-1", "1e30"} {
		item, err := parseProbe([]byte(`{"format":{"duration":"` + duration + `"},"streams":[{"codec_type":"video"}]}`))
		if err != nil || item.RunTimeTicks != 0 {
			t.Fatal("invalid duration became ticks")
		}
	}
}

func TestProbeProcess(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "coach-probe-helper" {
		return
	}
	switch os.Args[len(os.Args)-1] {
	case "overflow":
		_, _ = os.Stdout.Write([]byte(strings.Repeat("x", maxProbeOutput+1)))
	case "wait":
		time.Sleep(30 * time.Second)
	case "environment":
		if os.Getenv("FFREPORT") != "" {
			os.Exit(1)
		}
		_, _ = os.Stdout.WriteString("clean")
	}
	os.Exit(0)
}

func TestProbeExecutionLimits(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"overflow", "wait"} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := runProbe(ctx, binary, nil, "-test.run=TestProbeProcess", "coach-probe-helper", mode)
		cancel()
		if err == nil {
			t.Fatalf("%s limit not enforced", mode)
		}
	}
	t.Setenv("FFREPORT", "test-only-value")
	output, err := runProbe(context.Background(), binary, nil, "-test.run=TestProbeProcess", "coach-probe-helper", "environment")
	if err != nil || string(output) != "clean" {
		t.Fatal("child inherited report environment")
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := newProbe(context.Background()); err == nil {
		t.Fatal("missing ffprobe accepted")
	}
}
