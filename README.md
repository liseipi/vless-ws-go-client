# VLESS over WebSocket — Go 客户端

与你的 `vless-ws-go` 服务端配套的本地客户端。本质是一个**本地代理**：
系统 / 浏览器 / curl 等把流量发给这个本地端口，客户端把每一条连接封装成
标准 VLESS 请求，通过 WebSocket 转发给你的服务器，由服务器代为连接真实目标。

**同一个本地端口自动同时支持 SOCKS5 和 HTTP(S) 代理**，客户端会窥探连接的第一个
字节自动区分协议，不需要分别配置两个端口。

```
你的设备                         你的服务器                    目标网站
┌─────────────┐ SOCKS5/HTTP ┌───────────────┐  WS+VLESS  ┌───────────────┐  TCP  ┌────────┐
│ 浏览器/curl  │ ──────────► │ vless-ws-client│ ─────────► │ vless-ws-server│ ────► │ target │
└─────────────┘             └───────────────┘             └───────────────┘       └────────┘
```

## 目录结构

```
vless-ws-client/
├── go.mod / go.sum
├── main.go        # 入口 + 启动横幅
├── config.go      # 命令行参数 / 环境变量配置
├── vless.go       # VLESS 请求头编码（与服务端 vless.go 严格对应）
├── vlessconn.go   # WebSocket 拨号 + VLESS 握手，封装成标准 net.Conn
├── proxy.go       # 本地监听 + 协议自动识别调度 + SOCKS5 实现
├── http.go        # HTTP CONNECT 隧道 + 普通 HTTP 代理转发
├── log.go         # 分级日志，风格与服务端一致
└── dist/          # 预编译好的各平台可执行文件，开箱即用
```

## 构建

```bash
go mod tidy
go build -o vless-ws-client .
```

如果你的环境访问不了 `proxy.golang.org`，可以直接从 GitHub 拉依赖：

```bash
GOPROXY=direct GOSUMDB=off go mod tidy
go build -o vless-ws-client .
```

`dist/` 目录下已经提供了交叉编译好的可执行文件（Linux/macOS 的 amd64+arm64、
Windows amd64），下载对应平台的直接运行即可，不需要装 Go 环境。

## 交叉编译

不需要在目标机器上编译，本机装好 Go 就能生成其他平台的可执行文件，
只需设置 `GOOS`/`GOARCH` 环境变量：

```bash
# macOS Apple 芯片 (M1/M2/M3/M4)
GOOS=darwin GOARCH=arm64 go build -o vless-ws-client-darwin-arm64 .

# macOS Intel 芯片
GOOS=darwin GOARCH=amd64 go build -o vless-ws-client-darwin-amd64 .

# Linux amd64
GOOS=linux GOARCH=amd64 go build -o vless-ws-client-linux-amd64 .

# Linux arm64（树莓派、云服务器 ARM 机型等）
GOOS=linux GOARCH=arm64 go build -o vless-ws-client-linux-arm64 .

# Windows amd64
GOOS=windows GOARCH=amd64 go build -o vless-ws-client-windows-amd64.exe .
```

如果拉依赖时连不上 `proxy.golang.org`，同样加上 `GOPROXY=direct GOSUMDB=off`：

```bash
GOPROXY=direct GOSUMDB=off GOOS=linux GOARCH=amd64 go build -o vless-ws-client-linux-amd64 .
```

（Windows PowerShell 下环境变量语法不同，需要分两行设置，例如：
`$env:GOOS="windows"; $env:GOARCH="amd64"; go build -o vless-ws-client-windows-amd64.exe .`）

一次性编译全部平台，可以用下面的脚本：

```bash
#!/usr/bin/env bash
set -e
mkdir -p dist
platforms=(
  "linux amd64"
  "linux arm64"
  "darwin amd64"
  "darwin arm64"
  "windows amd64"
)
for p in "${platforms[@]}"; do
  read -r os arch <<< "$p"
  ext=""
  [ "$os" = "windows" ] && ext=".exe"
  echo "building $os/$arch..."
  GOOS=$os GOARCH=$arch go build -o "dist/vless-ws-client-${os}-${arch}${ext}" .
done
```

## 运行

### 使用 dist/ 预编译文件

`dist/` 目录下提供了各平台的可执行文件，macOS 用户首次运行前需要执行：

```bash
# 以 Apple 芯片 (arm64) 为例，Intel 芯片换成 darwin-amd64
chmod +x dist/vless-ws-client-darwin-arm64
xattr -d com.apple.quarantine dist/vless-ws-client-darwin-arm64

# 然后直接运行
./dist/vless-ws-client-darwin-arm64 \
  -host your-domain.com \
  -port 443 \
  -path /api \
  -uuid a3d2e1f0-b4c5-4d6e-8f70-1a2b3c4d5e6f \
  -token 你的TOKEN
```

Linux 和 Windows 用户不需要 `xattr` 步骤，`chmod +x` 后直接运行即可。

### 自行构建后运行

```bash
./vless-ws-client \
  -host your-domain.com \
  -port 443 \
  -path /api \
  -uuid a3d2e1f0-b4c5-4d6e-8f70-1a2b3c4d5e6f \
  -token 你的TOKEN \
  -local-ip 127.0.0.1 \
  -local-port 1080
```

也可以用环境变量：

```bash
SERVER_HOST=your-domain.com \
SERVER_PORT=443 \
WS_PATH=/api \
UUID=a3d2e1f0-b4c5-4d6e-8f70-1a2b3c4d5e6f \
TOKEN=你的TOKEN \
LOCAL_IP=127.0.0.1 \
LOCAL_PORT=1080 \
./vless-ws-client
```

### 参数说明

| 参数 | 环境变量 | 默认值 | 说明 |
|---|---|---|---|
| `-host` | `SERVER_HOST` | 必填 | 服务器地址（域名或 IP），**不要**带 `ws://` 或 `wss://` 前缀，例如 `example.com` |
| `-port` | `SERVER_PORT` | `443` | 服务器端口，例如 `443`、`8081` |
| `-path` | `WS_PATH` | `/api` | 连接路径，需与服务端 `WS_PATH` 一致 |
| `-tls` | `USE_TLS` | `true` | 是否使用 TLS（`wss://`）。生产环境（前面有 Nginx/Caddy 做 TLS）保持默认；直连裸端口调试时设为 `false` 用 `ws://` |
| `-local-ip` | `LOCAL_IP` | `127.0.0.1` | 本地监听 IP，只在本机使用请保持 `127.0.0.1`；要给局域网内其他设备用才改成 `0.0.0.0`，注意安全风险 |
| `-local-port` | `LOCAL_PORT` | `1080` | 本地监听端口，**SOCKS5 与 HTTP(S) 代理共用同一个端口**，自动识别协议 |
| `-uuid` | `UUID` | 必填 | 必须与服务端 `UUID` 完全一致 |
| `-token` | `TOKEN` | 空 | 服务端启用了 `TOKEN` 校验时必填，不带引号原样填入 |
| `-sni` | `SNI` | 空(等于 `-host`) | TLS ClientHello 的 server_name，一般不需要单独设置；只有直连 IP/CDN 节点但仍要保持正确域名路由时才用得上 |
| `-ws-host` | `WS_HOST` | 空(等于 `-host`) | HTTP 握手请求里的 `Host` 头，同上，一般留空即可 |
| `-connect-retries` | — | `2` | 建立隧道失败时的额外重试次数（不含首次尝试），指数退避 |
| `-retry-base-ms` | — | `300` | 重试退避基准时间（毫秒），第 n 次重试等待 `base × 2^(n-1)` 左右 |
| `-keepalive-sec` | — | `30` | TCP KeepAlive 探测间隔（秒），`0` 关闭 |
| `-insecure` | `INSECURE=1` | `false` | 跳过 TLS 证书校验，仅用于自签名证书调试，正式使用不要开 |
| `-dial-timeout-ms` | — | `10000` | 单次连接服务端的超时时间 |
| `-log-level` | `LOG_LEVEL` | `info` | `debug/info/warn/error` |

服务端最终连接地址由 `host:port/path` + `tls` 拼接得到，例如
`host=example.com port=443 path=/api tls=true` → `wss://example.com:443/api`。

### 连接可靠性

- **失败自动重试**：建立 VLESS/WS 隧道失败时（DNS 抖动、握手瞬时失败等）会按指数退避自动重试
  `-connect-retries` 次再放弃，减少偶发网络问题导致连接直接失败的情况。
- **TCP KeepAlive**：本地和上游连接都会开启 KeepAlive 探测，长时间空闲的隧道不容易被
  NAT/防火墙误判为死连接而被丢弃。
- **SNI / Host 解耦**：正常情况下不需要关心，`-sni`/`-ws-host` 默认都等于 `-host`；
  只有特殊网络环境（例如需要直连服务器 IP，但仍要按域名做 TLS/虚拟主机路由）才需要单独设置。
- 说明：隧道一旦建立成功、数据开始转发后，中途断开**不会**自动重连——VLESS 协议本身
  没有会话恢复能力，重连只是换一条新隧道连到同一目标，对已经在传输中的数据流没有意义，
  交给上层应用（浏览器/工具自身的重试逻辑）处理更安全，也不会有数据错乱的风险。

## 配合系统 / 浏览器使用

- **系统代理（macOS/Windows）**：SOCKS5 和 HTTP(S) 代理都填同一个地址
  `127.0.0.1:1080`（同一端口自动识别协议，不需要分别配置）。
- **curl 测试**：
  ```bash
  # 走 SOCKS5（域名解析也走隧道）
  curl --socks5-hostname 127.0.0.1:1080 https://example.com

  # 走 HTTP(S) 代理（同一个端口）
  curl -x http://127.0.0.1:1080 https://example.com
  ```
- **Clash / Shadowrocket 等**：只要支持"上游 SOCKS5"或"上游 HTTP 代理"的都可以接入，
  把本客户端当成一个普通的本地出口，两种协议都指向同一端口即可。

支持域名 / IPv4 / IPv6 三种目标地址类型，对应服务端 `atypDomain/atypIPv4/atypIPv6`。

## 已验证

- 完整握手 + SOCKS5 CONNECT + WebSocket 转发 + 目标 HTTP 响应往返
- 同一端口下 HTTP CONNECT（HTTPS 隧道）与普通 HTTP 代理转发均测试通过
- Token 校验、`go vet` 静态检查通过

## 注意事项

- SOCKS5 部分只实现了 `CONNECT` 命令（网页浏览、大多数应用够用），不支持 `BIND` / `UDP ASSOCIATE`。
- 普通（非 CONNECT）HTTP 代理转发为简化实现，每条连接只转发一个请求（强制 `Connection: close`），
  覆盖 `curl -x` 等基本场景；HTTPS 站点走的是 `CONNECT` 隧道，不受此限制，长连接、多请求都没问题。
- 每个代理连接对应一条独立的 WebSocket 连接（与服务端 `session.go` 的单连接单会话模型一致），
  高并发场景下注意服务端侧 `ulimit -n` 设置（见服务端 README）。
- 生产环境务必保持 `-tls=true`（配合 Nginx/Caddy 做 TLS 终止），`-insecure` 仅用于自签名证书调试。
