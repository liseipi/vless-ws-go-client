#!/usr/bin/env bash
# 交叉编译桌面平台（Linux / macOS / Windows），输出到 dist/。
# 纯 Go 代码，不需要 cgo / NDK，任意装了 Go 的机器上都能跑。
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

LDFLAGS="-s -w" # 去掉符号表和调试信息，减小体积

platforms=(
  "linux amd64 vless-ws-client-linux-amd64"
  "linux arm64 vless-ws-client-linux-arm64"
  "darwin amd64 vless-ws-client-darwin-amd64"
  "darwin arm64 vless-ws-client-darwin-arm64"
  "windows amd64 vless-ws-client-windows-amd64.exe"
)

for p in "${platforms[@]}"; do
  read -r os arch name <<< "$p"
  echo "==> building ${os}/${arch} -> ${OUT_DIR}/${name}"
  GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 go build -ldflags="$LDFLAGS" -o "${OUT_DIR}/${name}" .
done

echo ""
echo "全部完成，产物在 ${OUT_DIR}/ 目录下："
ls -la "$OUT_DIR"
