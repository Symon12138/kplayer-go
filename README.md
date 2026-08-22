<h1 align="center">KPlayer 播控台</h1>
<p align="center">无人值守视频直播推流引擎 + Web 管理控制台</p>

面向服务器环境：无需图形界面，登记素材、编排节目单、按计划自动开播，向多个平台同时推流。

## 核心能力

- **推流引擎**：基于 FFmpeg 子进程；每个推流任务独立引擎实例（独立 ffmpeg 路径），任务间互不干扰
- **多路分发**：一个任务多条输出线路，每条独立分辨率/码率/帧率/编码器（x264/x265/NVENC/QSV/AMF）/硬件加速/音视频滤镜；断线自动重推
- **内容编排**：媒体库（文件/目录/外挂音频字幕合并）、节目单（四种播放方式、后备节目单）
- **自动播出**：定时任务（每天固定时间或 cron）开播/关播，调度器可视可控
- **画面效果**：文字/图片水印、字幕烧录、跑马灯、画面调整、转码预设——真正注入 ffmpeg 命令行，作为所有线路滤镜的基底
- **运营闭环**：告警中心、Webhook 事件订阅、审计日志、数据统计（成功率/断流趋势/失败率）
- **高可用与规模**：输出分组、主备切换、健康策略、节点/实例/远程命令管理
- **复用与治理**：配置模板、行业模板一键部署、智能规则自动编排、建议审批、配置快照回滚、用户角色权限

## 架构

```
Web 播控台 (18 页, 静态 JS)
   │  /console/api 反向代理
   ▼
HTTP 管理 API ──┬─ 管理服务层 (management/)：媒体·节目单·任务·告警…
   │            └─ 推流引擎 (engine/ffmpeg.go)：每任务一个 ffmpeg 子进程
   ▼
FFmpeg（解码 → 滤镜合成 → 编码 → RTMP/文件多路输出）
```

## 快速开始

**Docker（推荐，已实测验证）**：

```bash
cp config.json.example config.json && docker compose up -d --build
# 打开 http://<服务器IP>:4156/console/
```

镜像内置 ffmpeg 与全部依赖；数据挂载 ./data、./video，重启不丢。
## 快速开始

```bash
# 1. 构建（Linux/WSL；Windows 见 native/README.md 的 stub 方案）
make build

# 2. 准备 config.json（参考 config.json.example），启动
./kplayer play start

# 3. 打开控制台
# http://<服务器IP>:4156/console/
```

Docker 部署：`docker-compose.yml` 已含数据目录挂载；重启不丢数据（`management.json`/`engine.json` 实时落盘）。

## 文档

- [使用指南](docs/使用指南.md)：18 个页面的操作说明（5 分钟上手）

## License

Apache-2.0
