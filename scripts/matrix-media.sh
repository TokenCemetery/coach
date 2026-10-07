#!/bin/sh
# Builds the synthetic media of the compatibility matrix (docs/COMPATIBILITY.md)
# in DIR. Each video is 120 s of testsrc2, whose frame shows its own
# timestamp, with keyframes every 2 s. Usage: matrix-media.sh DIR
set -eu

dir=${1:?usage: matrix-media.sh DIR}
mkdir -p "$dir"
video="-f lavfi -i testsrc2=s=640x360:d=120:r=25"
tone="-f lavfi -i sine=d=120:f=440"
second="-f lavfi -i sine=d=120:f=880"
key="-g 50 -keyint_min 50 -sc_threshold 0"

make() {
	out=$1
	shift
	# shellcheck disable=SC2086 # the option strings above are split on purpose
	ffmpeg -v error -nostdin -y "$@" "$dir/$out"
}

# shellcheck disable=SC2086
{
	make "MP4 H264 AAC.mp4" $video $tone -c:v libx264 $key -pix_fmt yuv420p -c:a aac
	make "MKV H264 AC3.mkv" $video $tone -c:v libx264 $key -pix_fmt yuv420p -c:a ac3
	make "MKV H264 two AAC.mkv" $video $tone $second -map 0 -map 1 -map 2 -c:v libx264 $key -pix_fmt yuv420p -c:a aac \
		-metadata:s:a:0 language=eng -metadata:s:a:1 language=rus
	make "TS H264 AAC.ts" $video $tone -c:v libx264 $key -pix_fmt yuv420p -c:a aac
	make "WebM VP9 Opus.webm" $video $tone -c:v libvpx-vp9 -deadline realtime -cpu-used 8 -g 50 -c:a libopus
	make "AVI MPEG4 MP3.avi" $video $tone -c:v mpeg4 -q:v 5 -g 50 -c:a libmp3lame
	make "MKV HEVC10 AC3.mkv" $video $tone -c:v libx265 -pix_fmt yuv420p10le -x265-params log-level=error:keyint=50:min-keyint=50:scenecut=0 -c:a ac3
}
# A sidecar subtitle with one 1.5 s cue every 2 s.
i=0
: >"$dir/MKV H264 AC3.srt"
while [ $i -lt 60 ]; do
	s=$((i * 2))
	printf '%d\n00:%02d:%02d,000 --> 00:%02d:%02d,500\nCue %d\n\n' $((i + 1)) $((s / 60)) $((s % 60)) $(((s + 1) / 60)) $(((s + 1) % 60)) $((i + 1)) >>"$dir/MKV H264 AC3.srt"
	i=$((i + 1))
done
