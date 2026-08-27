#!/usr/bin/env bash
# 把 dist-android/ 下已经编译好的 4 个架构二进制，按 Android 项目
# jniLibs 的目录规范复制一份并改名为 libvlessclient.so。
#
# 这是沿用项目里原有的打包手法：把普通可执行文件放进
# app/src/main/jniLibs/<ABI>/libxxx.so，Android 打包 APK 时会
# 按 ABI 把对应文件解压到 nativeLibraryDir 并带上可执行权限，
# 这样就不需要真的写 JNI 代码，纯粹是借用这个机制分发二进制。
#
# 依赖：必须先跑完 build-dist-android.sh，dist-android/ 下要有
# 4 个架构的产物，本脚本只做复制+改名，不会重新编译。
#
# 用法：
#   chmod +x build-jnilibs.sh
#   ./build-jnilibs.sh

set -e

SRC_DIR="dist-android"
OUT_DIR="jniLibs"

if [ ! -d "$SRC_DIR" ]; then
  echo "错误：找不到 $SRC_DIR 目录，请先运行 build-dist-android.sh 生成 Android 二进制"
  exit 1
fi

# 用普通数组 + case 而不是关联数组（declare -A），
# 因为 macOS 自带的 bash 是 3.2（不支持关联数组，那是 bash 4+ 才有的特性）。
abis=("arm64-v8a" "armeabi-v7a" "x86" "x86_64")

src_name_for_abi() {
  case "$1" in
    arm64-v8a)   echo "vless-ws-client-arm64-v8a" ;;
    armeabi-v7a) echo "vless-ws-client-armeabi-v7a" ;;
    x86)         echo "vless-ws-client-x86" ;;
    x86_64)      echo "vless-ws-client-x86_64" ;;
  esac
}

for abi in "${abis[@]}"; do
  src_file="${SRC_DIR}/$(src_name_for_abi "$abi")"
  dst_dir="${OUT_DIR}/${abi}"
  dst_file="${dst_dir}/libvlessclient.so"

  if [ ! -f "$src_file" ]; then
    echo "跳过 ${abi}：没找到 ${src_file}（该架构可能还没编译）"
    continue
  fi

  mkdir -p "$dst_dir"
  cp "$src_file" "$dst_file"
  chmod +x "$dst_file"
  echo "==> ${src_file} -> ${dst_file}"
done

echo ""
echo "全部完成，产物在 ${OUT_DIR}/ 目录下："
find "$OUT_DIR" -type f -exec ls -la {} \;
