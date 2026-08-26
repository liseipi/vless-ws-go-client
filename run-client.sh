#!/usr/bin/env bash
# 编译 + 运行客户端，参数从 client.env 读，不用每次在命令行手打。
#
# 首次使用：
#   cp client.env.example client.env
#   vim client.env   # 填入你自己的 host / uuid / token 等
#   chmod +x run-client.sh
#   ./run-client.sh
#
# 以后改配置只需要编辑 client.env，重新跑 ./run-client.sh 即可
#（脚本每次都会重新编译，保证跑的是最新代码）。
#
# 如果拉依赖时连不上 proxy.golang.org，取消下面这行注释：
# export GOPROXY=direct GOSUMDB=off

set -euo pipefail

ENV_FILE="client.env"
BIN_NAME="vless-ws-client"

info()  { echo -e "\033[36m[run-client]\033[0m $*"; }
error() { echo -e "\033[31m[run-client]\033[0m $*" >&2; }

if [ ! -f "./main.go" ]; then
  error "请在项目根目录（含 main.go 的目录）下运行本脚本"
  exit 1
fi

if [ ! -f "$ENV_FILE" ]; then
  error "找不到 ${ENV_FILE}，请先执行："
  error "  cp client.env.example ${ENV_FILE}"
  error "然后编辑 ${ENV_FILE} 填入你自己的配置"
  exit 1
fi

# shellcheck source=/dev/null
source "$ENV_FILE"

# 必填项检查，缺了直接报错退出，避免带着空值跑起来才发现连不上
missing=()
[ -z "${SERVER_HOST:-}" ] && missing+=("SERVER_HOST")
[ -z "${UUID:-}" ] && missing+=("UUID")
if [ ${#missing[@]} -gt 0 ]; then
  error "client.env 里缺少必填项：${missing[*]}"
  exit 1
fi

# 可选项给默认值，跟 config.go 里的默认值保持一致
SERVER_PORT="${SERVER_PORT:-443}"
WS_PATH="${WS_PATH:-/api}"
LOCAL_IP="${LOCAL_IP:-127.0.0.1}"
LOCAL_PORT="${LOCAL_PORT:-1080}"
USE_TLS="${USE_TLS:-true}"
INSECURE="${INSECURE:-false}"
LOG_LEVEL="${LOG_LEVEL:-info}"

info "拉取依赖 (go mod tidy) ..."
go mod tidy

info "编译 ${BIN_NAME} ..."
go build -o "${BIN_NAME}" .

info "启动客户端：${SERVER_HOST}:${SERVER_PORT}${WS_PATH} -> 本地 ${LOCAL_IP}:${LOCAL_PORT}"
echo ""

exec ./"${BIN_NAME}" \
  -host "${SERVER_HOST}" \
  -port "${SERVER_PORT}" \
  -path "${WS_PATH}" \
  -uuid "${UUID}" \
  -token "${TOKEN:-}" \
  -local-ip "${LOCAL_IP}" \
  -local-port "${LOCAL_PORT}" \
  -tls="${USE_TLS}" \
  -insecure="${INSECURE}" \
  -log-level "${LOG_LEVEL}"
