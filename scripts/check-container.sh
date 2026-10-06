#!/bin/sh
# Smoke-checks a Coach container image: -init, scan with FFprobe, and HLS remux
# of an H.264 + AC3 movie through FFmpeg. Usage: check-container.sh IMAGE
# CONTAINER selects the engine (docker or podman, default docker).
set -eu

image=${1:?usage: check-container.sh IMAGE}
engine=${CONTAINER:-docker}
name=coach-check-$$
port=18097

cleanup() {
	"$engine" rm -f "$name" >/dev/null 2>&1 || true
	"$engine" volume rm -f "$name-data" "$name-media" >/dev/null 2>&1 || true
}
trap cleanup EXIT

fail() {
	echo "FAIL: $*" >&2
	"$engine" logs "$name" >&2 2>&1 || true
	exit 1
}

"$engine" run --rm --entrypoint ffprobe "$image" -hide_banner -protocols | grep -qx '  fd' || fail "ffprobe lacks the fd protocol"

"$engine" run --rm --user root -v "$name-media:/media" --entrypoint ffmpeg "$image" \
	-v error -nostdin -f lavfi -i testsrc2=size=320x240:rate=25 -f lavfi -i sine=frequency=440 \
	-t 20 -c:v libx264 -g 50 -c:a ac3 /media/Movie.mkv

password=$(head -c 18 /dev/urandom | base64)
printf '%s\n' "$password" | "$engine" run --rm -i -v "$name-data:/data" "$image" -init -username viewer
"$engine" run -d --name "$name" -p "127.0.0.1:$port:8097" -v "$name-data:/data" -v "$name-media:/media:ro" \
	"$image" -listen :8097 -media-dir /media >/dev/null

base=http://127.0.0.1:$port
for _ in $(seq 30); do
	curl -sf "$base/healthz" >/dev/null && break
	sleep 1
done
curl -sf "$base/healthz" >/dev/null || fail "server did not start"
if "$engine" logs "$name" 2>&1 | grep -q 'FFmpeg not found'; then
	fail "HLS remux is disabled"
fi

auth='Emby Client="check", Device="check", DeviceId="check-1", Version="1"'
token=$(jq -n --arg pw "$password" '{Username: "viewer", Pw: $pw}' |
	curl -sf -H "Authorization: $auth" -H 'Content-Type: application/json' -d @- "$base/Users/AuthenticateByName" |
	jq -r .AccessToken)
item=$(curl -sf "$base/Items?Recursive=true&IncludeItemTypes=Movie&api_key=$token" | jq -r '.Items[0].Id')
[ "$item" != null ] || fail "the movie was not scanned"

profile='{"DeviceProfile":{"MaxStreamingBitrate":120000000,
 "DirectPlayProfiles":[{"Type":"Video","Container":"mp4,mkv","VideoCodec":"h264","AudioCodec":"aac"}],
 "TranscodingProfiles":[{"Type":"Video","Container":"ts","Protocol":"hls","VideoCodec":"h264","AudioCodec":"aac","Context":"Streaming"}]}}'
url=$(curl -sf -H 'Content-Type: application/json' -d "$profile" "$base/Items/$item/PlaybackInfo?api_key=$token" |
	jq -r '.MediaSources[0].TranscodingUrl // empty')
case $url in
*master.m3u8*) ;;
*) fail "PlaybackInfo did not choose HLS" ;;
esac

streams=$("$engine" exec "$name" ffprobe -v error -show_entries stream=codec_name -of csv=p=0 "http://127.0.0.1:8097$url" | grep . | sort -u | tr '\n' ' ')
[ "$streams" = "aac h264 " ] || fail "HLS streams are '$streams', want aac and h264"

"$engine" stop -t 10 "$name" >/dev/null
[ "$("$engine" inspect "$name" --format '{{.State.ExitCode}}')" = 0 ] || fail "non-zero exit on stop"
echo "OK: $image"
