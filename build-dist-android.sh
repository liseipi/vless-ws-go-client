#!/usr/bin/env bash
# 交叉编译 Android 4 个架构，输出到 dist-android/。
#
# 除了 arm64-v8a 之外，其余 3 个架构（armeabi-v7a / x86 / x86_64）
# Go 要求走 cgo 外部链接，必须要有 Android NDK 的交叉编译工具链，
# 所以运行前必须先装好 NDK 并设置好 ANDROID_NDK_HOME。
#
# 用法：
#   1. 安装 Android NDK（可以通过 Android Studio 的 SDK Manager 装，
#      或者去 https://developer.android.com/ndk/downloads 单独下载）
#   2. export ANDROID_NDK_HOME=/path/to/android-ndk   # 换成你机器上的实际路径
#   3. chmod +x build-dist-android.sh
#   4. ./build-dist-android.sh
#
# 查看ndk: ls ~/Library/Android/sdk/ndk/
# 如果拉依赖时连不上 proxy.golang.org，取消下面这行注释：
# export GOPROXY=direct GOSUMDB=off

set -e

if [ -z "$ANDROID_NDK_HOME" ]; then
  echo "错误：请先设置 ANDROID_NDK_HOME 环境变量，指向你的 Android NDK 安装目录"
  echo "例如：export ANDROID_NDK_HOME=\$HOME/Android/Sdk/ndk/26.1.10909125"
  exit 1
fi

# NDK 的 clang 工具链目录，macOS/Linux 下一般是这个路径；
# 如果你的系统是别的（比如 windows-x86_64），把下面这行换掉。
HOST_TAG="linux-x86_64"
case "$(uname -s)" in
  Darwin) HOST_TAG="darwin-x86_64" ;;
  Linux)  HOST_TAG="linux-x86_64" ;;
esac

TOOLCHAIN="$ANDROID_NDK_HOME/toolchains/llvm/prebuilt/$HOST_TAG/bin"
if [ ! -d "$TOOLCHAIN" ]; then
  echo "错误：没找到 NDK 工具链目录：$TOOLCHAIN"
  echo "请确认 ANDROID_NDK_HOME 设置正确，以及 HOST_TAG 是否匹配你的操作系统"
  exit 1
fi

# API_LEVEL 21 覆盖到 Android 5.0+，一般够用；如需支持更低版本自行调整。
API_LEVEL=21

OUT_DIR="dist-android"
mkdir -p "$OUT_DIR"

# 跟 build-dist.sh / run-client.sh 保持一致：编译前先 tidy 一遍依赖。
echo "==> go mod tidy"
go mod tidy

LDFLAGS="-s -w"

echo "==> building android/arm64 (arm64-v8a)"
CGO_ENABLED=1 GOOS=android GOARCH=arm64 \
  CC="$TOOLCHAIN/aarch64-linux-android${API_LEVEL}-clang" \
  go build -ldflags="$LDFLAGS" -o "${OUT_DIR}/vless-ws-client-arm64-v8a" .

echo "==> building android/arm (armeabi-v7a)"
CGO_ENABLED=1 GOOS=android GOARCH=arm GOARM=7 \
  CC="$TOOLCHAIN/armv7a-linux-androideabi${API_LEVEL}-clang" \
  go build -ldflags="$LDFLAGS" -o "${OUT_DIR}/vless-ws-client-armeabi-v7a" .

echo "==> building android/386 (x86)"
CGO_ENABLED=1 GOOS=android GOARCH=386 \
  CC="$TOOLCHAIN/i686-linux-android${API_LEVEL}-clang" \
  go build -ldflags="$LDFLAGS" -o "${OUT_DIR}/vless-ws-client-x86" .

echo "==> building android/amd64 (x86_64)"
CGO_ENABLED=1 GOOS=android GOARCH=amd64 \
  CC="$TOOLCHAIN/x86_64-linux-android${API_LEVEL}-clang" \
  go build -ldflags="$LDFLAGS" -o "${OUT_DIR}/vless-ws-client-x86_64" .

echo ""
echo "全部完成，产物在 ${OUT_DIR}/ 目录下："
ls -la "$OUT_DIR"