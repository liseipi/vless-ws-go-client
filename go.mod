module vless-ws-client

go 1.22.2

require github.com/coder/websocket v1.8.12

require github.com/hashicorp/yamux v0.1.2

// tun 模式依赖：wireguard-go 提供跨平台 tun 网卡创建，
// github.com/sagernet/gvisor 是 gvisor.dev/gvisor 的社区维护镜像
// （sing-box/sing-tun 同款依赖），修掉了上游仓库那套需要 Bazel
// 代码生成才能编译的伪泛型代码，可以直接用标准 `go build` 编译。
require (
	github.com/sagernet/gvisor v0.0.0-20230930221345-5fef6f2e17ab
	golang.zx2c4.com/wireguard v0.0.0-20230223191233-e24fc776e0ff
)
