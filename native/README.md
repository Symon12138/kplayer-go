# native/ — 模拟核心（stub）构建与部署说明

KPlayer 的 C++ 核心（libkplayer）闭源分发：官方只发布静态链接的二进制
（GitHub/gitee 仓库无源码，Docker 镜像 `bytelang/kplayer:latest` 内也只有
`/usr/bin/kplayer`，无 `.so` 与 `extra.h`）。本目录提供 API 兼容的模拟引擎，
使本项目的 Go 控制层（core/kplayer.go 的 13 个 cgo 入口）可以真实编译运行，
完整跑通 Web 控制台 / REST / 调度器等全部管理层功能。**不执行真实编码推流。**

## 文件

- `extra.h` — 与官方一致的 C 头文件（从 core/kplayer.go 的 cgo 调用反推签名）
- `libkplayer_stub.c` — 模拟引擎：异步回调（20ms 延时线程，避免 keeper 通道
  同 goroutine 死锁）；带状态记忆（资源/输出/插件），列表与 current 查询返回
  与 proto 字段名一致的合法 JSON（jsonpb 遇未知字段会 log.Fatal，body 必须精确）

## 构建（WSL / Linux）

```bash
cd native
gcc -shared -fPIC -o libkplayer.so libkplayer_stub.c -lpthread
# 其余 4 个链接库为空占位
for lib in libkpcodec libkputil libkpadapter libkpplugin; do
  echo "" | gcc -shared -fPIC -x c - -o $lib.so
done
```

## 构建 Go 二进制（动态链接 stub）

```bash
export CGO_ENABLED=1
export CGO_CFLAGS="-I$HOME/kplayer-go/native"
export CGO_LDFLAGS="-L$HOME/kplayer-go/native -Wl,-rpath,$HOME/kplayer-go/native"
go build -o kplayer \
  -ldflags "-X github.com/bytelang/kplayer/types.CipherKey=0123456789abcdef0123456789abcdef \
            -X github.com/bytelang/kplayer/types.CipherIV=0123456789abcdef" .
./kplayer play start
```

## 运行前提（stub 环境）

- `config.json` 的 `resource.lists` 指向真实存在的文件（stub 不做内容校验，
  Go 侧仅 `os.Stat` 存在性检查）
- 为空配置做了 4 处防御性修复（已合入上游代码）：`types/tls.go` 空证书跳过、
  `types/api.go` 空证书池回退普通 Client、`module/plugin/provider/provider.go`
  字体资源初始化失败降级、`module/play/provider/command.go` 空 ApiHost 跳过 knock
- 回调消息格式：`ReceiveMessage(int action, char *body)`，body 是消息体本身
  （Go 侧 `goCallBackMessage` 自行组装 KPMessage，不要再包 `{"action":N,...}` 信封）

## 与真实库的差异

- 无解码/编码/推流；所有 prompt 收到后 20ms 异步回合法空消息
- `GetInformation` 返回空版本信息；`Run` 空转
- RESOURCE_CURRENT 的 resource.path 仅在真实开播流程（addNextResourceToCore
  发 RESOURCE_ADD prompt）后才有值；REST 层 ResourceAdd 只入 Go 侧队列
