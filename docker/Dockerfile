# KPlayer (five-phase edition) — self-contained image with the stub core,
# the real FFmpeg playback engine and the embedded management console.
# Build from the repository root after building the kplayer binary:
#   (in WSL, with the native stub built)
#   go build -ldflags "-X github.com/bytelang/kplayer/types.CipherKey=0123456789abcdef0123456789abcdef -X github.com/bytelang/kplayer/types.CipherIV=0123456789abcdef" -o kplayer .
#   mkdir -p docker/ctx && cp kplayer docker/ctx/ && cp native/libkplayer.so docker/ctx/
#   cp <ffmpeg> docker/ctx/ffmpeg && cp <ffprobe> docker/ctx/ffprobe
#   docker build -t kplayer-go:latest -f docker/Dockerfile docker/ctx
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates tzdata \
    && rm -rf /var/lib/apt/lists/*
ENV LD_LIBRARY_PATH=/usr/local/kplayer/lib
ENV TZ=Asia/Shanghai
COPY kplayer /usr/local/kplayer/kplayer
COPY libkplayer.so /usr/local/kplayer/lib/libkplayer.so
COPY ffmpeg /usr/local/bin/ffmpeg
COPY ffprobe /usr/local/bin/ffprobe
COPY config.json /kplayer/config.json
WORKDIR /kplayer
EXPOSE 4155 4156
CMD ["/usr/local/kplayer/kplayer", "play", "start"]
