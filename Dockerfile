# ---- 构建阶段：Go + gcc（编译原生存根库与二进制）----
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY . .
RUN cd native && \
    gcc -shared -fPIC -o libkplayer.so libkplayer_stub.c -lpthread && \
    for lib in libkpcodec libkputil libkpadapter libkpplugin; do \
      echo "" | gcc -shared -fPIC -x c - -o $$lib.so; \
    done
RUN CGO_ENABLED=1 \
    CGO_CFLAGS="-I/src/native" \
    CGO_LDFLAGS="-L/src/native -Wl,-rpath,/kplayer" \
    go build -o /out/kplayer .

# ---- 运行阶段：debian + ffmpeg（真实推流）----
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg ca-certificates && rm -rf /var/lib/apt/lists/*
WORKDIR /kplayer
COPY --from=build /out/kplayer /kplayer/kplayer
COPY --from=build /src/native/*.so /kplayer/
ENV LD_LIBRARY_PATH=/kplayer
EXPOSE 4155 4156
CMD ["./kplayer", "play", "start"]