# KPlayer Docker 部署（五阶段版）

自包含镜像：stub 核心 + 真实 FFmpeg 播放引擎 + 内嵌 Web 控制台（无需登录直入）。

## 构建

先按仓库说明在 Linux/WSL 构建 kplayer 二进制（含 native stub 与 ldflags），
并准备 FFmpeg 静态版：

```bash
mkdir -p docker/ctx
cp kplayer docker/ctx/
cp native/libkplayer.so docker/ctx/
cp /path/to/ffmpeg docker/ctx/ffmpeg
cp /path/to/ffprobe docker/ctx/ffprobe
cp config.json docker/ctx/config.json   # 默认指向 /video/example.mp4
docker build -t kplayer-go:latest -f docker/Dockerfile docker/ctx
```

## 运行（配置持久化）

```bash
mkdir -p data video
# 放一个视频到 video/（与 config.json 的资源路径一致）
cp ctx/config.json data/config.json   # 数据目录需含 config.json
docker compose -f docker/docker-compose.yml up -d
# 控制台: http://<host>:4156/console/  （API: 4155 gRPC / 4156 HTTP）
```

**持久化说明**：`./data` 目录挂载到容器工作目录 `/kplayer`——
`config.json` 由你放入；`management.json`（媒体/节目单/任务/用户等）、
`engine.json`（引擎配置）由系统自动生成并落盘。容器重启、删除重建后配置全部保留
（已实测：创建配置 → docker restart → docker rm + 重新 run，数据完整）。
注意不要使用「目录挂载 + 嵌套文件挂载」的组合
（如 `-v ./data:/kplayer -v ./config.json:/kplayer/config.json`），Docker 的嵌套处理不可靠。

## 验证过的能力（WSL + SRS 实测）

- 容器启动即服务（gRPC 4155 / HTTP 4156），控制台免登录直入
- 媒体注册 → 引擎配置（ffmpegPath=/usr/local/bin/ffmpeg）→ 播放 → 真实推流
  rtmp://127.0.0.1:1935/live/docker（H264 640x360 + MP3）→ SRS 收流 → ffprobe 拉流成功
- 停止后 SRS 流移除；容器保持运行

## 推流到本机 SRS

同一台机器上跑 SRS 容器时，用 host 网络（compose 中 `network_mode: "host"`
已注释）或把 engine 输出地址指向 SRS 容器名（docker network 互联）。
