#!/usr/bin/env bash
# 交叉编译桌面平台（Linux / macOS / Windows），输出到 dist/。
#
# 用法：
#   chmod +x build-dist.sh
#   ./build-dist.sh
#
# 如果拉依赖时连不上 proxy.golang.org，取消下面这行注释：
# export GOPROXY=direct GOSUMDB=off

set -e

OUT_DIR="dist"
mkdir -p "$OUT_DIR"

# 跟 run-client.sh 保持一致：编译前先 tidy 一遍依赖，避免 go.mod/go.sum
# 状态跟 run-client.sh 编译时不一致（哪怕理论上不该有实质影响，也顺手对齐）。
echo "==> go mod tidy"
go mod tidy

LDFLAGS="-s -w" # 去掉符号表和调试信息，减小体积；跨 OS 的 linux/windows 用这个没问题

# macOS 目标：必须让 CGO_ENABLED=1，否则在 Darwin 上 Go 会退回纯 Go 的 DNS
# 解析器，绕开系统的 mDNSResponder，不认识 /etc/resolver、search domain、
# VPN 分流 DNS 这些 macOS 特有的解析规则——域名解析可能解析到不理想的 IP，
# 实际连接效果会明显变差，但看起来"编译没报错、程序也能跑"，很容易被忽略。
# 只要是在真正的 Mac 上编译（装了 Xcode 命令行工具），amd64/arm64 互相
# 交叉编译 cgo 是原生支持的（苹果通用二进制机制自带），不需要额外工具链，
# 所以这两个目标不需要关 cgo。
#
# 这两个目标特意不加 -ldflags，跟 run-client.sh（本地原生 `go build .`，
# 什么 ldflags 都没加）保持完全一致的编译参数，排除"只是符号表不同"这种
# 干扰变量，方便你直接对比两边产物是否真的等价。
echo "==> building darwin/amd64 -> ${OUT_DIR}/vless-ws-client-darwin-amd64 (cgo enabled, 无 ldflags，与 run-client.sh 一致)"
GOOS=darwin GOARCH=amd64 CGO_ENABLED=1 go build -o "${OUT_DIR}/vless-ws-client-darwin-amd64" .

echo "==> building darwin/arm64 -> ${OUT_DIR}/vless-ws-client-darwin-arm64 (cgo enabled, 无 ldflags，与 run-client.sh 一致)"
GOOS=darwin GOARCH=arm64 CGO_ENABLED=1 go build -o "${OUT_DIR}/vless-ws-client-darwin-arm64" .

echo "==> building linux/amd64 -> ${OUT_DIR}/vless-ws-client-linux-amd64 (cgo disabled，跨 OS 交叉编译)"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="$LDFLAGS" -o "${OUT_DIR}/vless-ws-client-linux-amd64" .

echo "==> building linux/arm64 -> ${OUT_DIR}/vless-ws-client-linux-arm64 (cgo disabled，跨 OS 交叉编译)"
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags="$LDFLAGS" -o "${OUT_DIR}/vless-ws-client-linux-arm64" .

echo "==> building windows/amd64 -> ${OUT_DIR}/vless-ws-client-windows-amd64.exe (cgo disabled，跨 OS 交叉编译)"
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="$LDFLAGS" -o "${OUT_DIR}/vless-ws-client-windows-amd64.exe" .

echo ""
echo "全部完成，产物在 ${OUT_DIR}/ 目录下："
ls -la "$OUT_DIR"
