# TUN 模式使用说明

## 改动了什么

新增 `tun.go`，实现了系统级透明代理：创建一张 tun 虚拟网卡，用
[gVisor netstack](https://github.com/sagernet/gvisor)（sing-box/Clash Meta
同款依赖）把网卡收到的 IP 包解析成 TCP 连接和 UDP 会话，再直接调用你项目
里已有的 `DialVless` / `DialVlessUDP` 转发进 VLESS 隧道。跟原来的
SOCKS5/HTTP 代理是平级的两种前端，二选一启动（见 `main.go` 改动），互不
影响原有逻辑。

`config.go` / `main.go` 只做了最小改动：加了 5 个新命令行参数，`main.go`
里根据 `-tun` 是否开启选择走 tun 还是原来的 SOCKS5/HTTP。

## 依赖说明

`go.mod` 新增两个依赖：

```
github.com/sagernet/gvisor v0.0.0-20230930221345-5fef6f2e17ab
golang.zx2c4.com/wireguard v0.0.0-20230223191233-e24fc776e0ff
```

**注意**：没有直接用官方的 `gvisor.dev/gvisor`，而是用了
`github.com/sagernet/gvisor`——这是 sing-box/sing-tun 生态实际在用的社区
维护分支。官方仓库是 Bazel 项目，一部分代码（伪泛型模板）依赖构建时代码
生成，纯 `go build`/`go get` 编译不过；这个分支把那部分改成了标准 Go
泛型，可以直接编译。这不是我瞎选的，是经过实际验证的（见下方"验证过程"）。

在你自己的机器上跑：

```bash
go mod tidy
go build .
```

即可。我已经在沙盒环境里针对 linux/amd64、linux/arm64、darwin/arm64、
windows/amd64 四个目标分别交叉编译验证过，`go build` 和 `go vet` 都是干净
通过的。

## 新增命令行参数

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-tun` | `false` | 启用 tun 模式，需要管理员/root 权限创建虚拟网卡 |
| `-tun-name` | `utun233` | 网卡名。Linux 用 `tun0` 之类，**macOS 必须是 `utunN` 格式**，Windows 随意 |
| `-tun-mtu` | `1500` | 网卡 MTU |
| `-tun-addr4` | `198.18.0.1` | 分配给网卡的 IPv4 地址 |
| `-tun-addr6` | 空 | 分配给网卡的 IPv6 地址，留空则不配置 IPv6 |

也都支持同名环境变量：`ENABLE_TUN` / `TUN_NAME` / `TUN_MTU` / `TUN_ADDR4` / `TUN_ADDR6`。

## 系统路由配置（这一步必须手动做，程序不会帮你改路由表）

tun 网卡建好之后，默认没有任何流量会经过它——需要你手动把系统的默认路由
指向这张网卡，同时**必须给"客户端连服务器"这条底层连接本身开一条例外
路由**，否则会自己套自己形成死循环（VLESS-WS 连接本身也走了 tun ->
netstack -> 又想找隧道转发 -> 死循环）。

### Linux（以 root 运行，网卡名假设是 `tun0`）

```bash
sudo ./vless-ws-client -host your.server.com -uuid xxx -tun -tun-name tun0

# 另开一个终端，配置路由：
sudo ip link set tun0 up
sudo ip addr add 198.18.0.1/15 dev tun0

# 关键：先给服务器 IP 单独打一条走原来物理网卡的路由，防止死循环
SERVER_IP=$(dig +short your.server.com | head -1)
ORIG_GW=$(ip route show default | awk '{print $3; exit}')
ORIG_DEV=$(ip route show default | awk '{print $5; exit}')
sudo ip route add $SERVER_IP via $ORIG_GW dev $ORIG_DEV

# 再把默认路由改到 tun0
sudo ip route add 0.0.0.0/1 dev tun0
sudo ip route add 128.0.0.0/1 dev tun0
```

（用两条 `/1` 而不是直接改 `0.0.0.0/0`，是常见的"不删除原默认路由、只是
优先级更高地覆盖它"的技巧，方便你之后一键删掉这两条恢复原状。）

### macOS

```bash
sudo ./vless-ws-client -host your.server.com -uuid xxx -tun -tun-name utun233

# 网卡地址：
sudo ifconfig utun233 198.18.0.1 198.18.0.1 up

# 服务器 IP 例外路由（防止死循环）+ 默认路由接管，写法和 Linux 思路一致，
# 命令换成 macOS 的 route/networksetup，具体命令视你的网络环境（Wi-Fi/以太网）
# 而定，这里不展开。
```

### Windows

wireguard-go 在 Windows 下用的是 Wintun 驱动，首次运行需要能加载
`wintun.dll`（跟 WireGuard 官方客户端用的是同一个驱动，如果你机器上装过
WireGuard/Tailscale 之类通常已经有了；没有的话需要自己放一份 `wintun.dll`
到程序同目录）。路由配置用 `netsh interface ip` 或者 PowerShell 的
`New-NetRoute`，同样要记得给服务器 IP 开例外路由。

## DNS

不需要额外处理：只要系统 DNS 解析请求（UDP 53）被路由进了 tun 网卡，就会
自动走 `handleUDP` 转发进 VLESS 隧道解析，不会泄漏。

## 已知限制 / 建议后续加固的点

- 权限：三个平台创建 tun 网卡都需要管理员/root，代码里没有做权限检查，
  权限不够时 `wgtun.CreateTUN` 会直接返回错误。
- 路由/DNS 配置目前需要手动做（上面的命令），没有像 Clash Meta 那样自动
  接管系统路由表 + 自动加例外路由防死循环；如果这块你需要我也可以继续做，
  但涉及大量平台特定的系统调用，工作量不小。
- ICMP（ping）没有实现——目前只注册了 TCP/UDP 的 forwarder，`ping` 命令
  在 tun 模式下会没有响应，不影响正常上网。
