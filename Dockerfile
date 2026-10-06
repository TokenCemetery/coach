# Release image: static Coach plus FFmpeg/FFprobe for scanning and HLS.
# Alpine's FFmpeg has the fd protocol Coach requires and libx264/aac.

FROM docker.io/library/golang:1.27-alpine3.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/coach ./cmd/coach

FROM docker.io/library/alpine:3.24
RUN apk add --no-cache ffmpeg \
    && adduser -D -H -u 10001 coach \
    && mkdir /data \
    && chown coach:coach /data \
    && chmod 0700 /data
COPY --from=build /out/coach /usr/local/bin/coach
USER coach
VOLUME /data
EXPOSE 8097
ENTRYPOINT ["coach", "-data", "/data"]
CMD ["-listen", ":8097"]
