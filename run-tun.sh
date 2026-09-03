#!/usr/bin/env bash
# 编译 + 以 tun 模式运行客户端（系统级透明代理），参数同样从 client.env 读。
#
# 跟 run-client.sh 用的是同一个 client.env：连接服务器需要的
# SERVER_HOST/UUID/TOKEN 等还是那几项，另外可以在 client.env 里加几个
# TUN_* 变量微调 tun 网卡（不加就用默认值，见下面）。
#
# 首次使用：
#   cp client.env.example client.env   # 如果还没做过
#   vim client.env                     # 填入 host / uuid / token
#   chmod +x run-tun.sh
#   sudo ./run-tun.sh                  # 创建 tun 网卡需要 root/管理员权限
#
# 跑起来之后，网卡建好了但系统流量还不会自动经过它——按脚本最后打印的提示
# （或者 TUN-SETUP.md）手动配置一下路由表。
#
# 如果拉依赖时连不上 proxy.golang.org，取消下面这行注释：
# export GOPROXY=direct GOSUMDB=off

set -euo pipefail

ENV_FILE="client.env"
BIN_NAME="vless-ws-client"

info()  { echo -e "\033[36m[run-tun]\033[0m $*"; }
warn()  { echo -e "\033[33m[run-tun]\033[0m $*"; }
error() { echo -e "\033[31m[run-tun]\033[0m $*" >&2; }

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

# tun 模式要创建虚拟网卡，必须有 root/管理员权限
if [ "$(id -u)" -ne 0 ]; then
  error "tun 模式需要 root 权限创建虚拟网卡，请用 sudo 重新运行："
  error "  sudo ./run-tun.sh"
  exit 1
fi

# shellcheck source=/dev/null
source "$ENV_FILE"

missing=()
[ -z "${SERVER_HOST:-}" ] && missing+=("SERVER_HOST")
[ -z "${UUID:-}" ] && missing+=("UUID")
if [ ${#missing[@]} -gt 0 ]; then
  error "client.env 里缺少必填项：${missing[*]}"
  exit 1
fi

# 连接相关，跟 run-client.sh 一致
SERVER_PORT="${SERVER_PORT:-443}"
WS_PATH="${WS_PATH:-/api}"
USE_TLS="${USE_TLS:-true}"
INSECURE="${INSECURE:-false}"
LOG_LEVEL="${LOG_LEVEL:-info}"

# tun 网卡相关，跟 config.go 里的默认值一致
TUN_NAME="${TUN_NAME:-utun233}"
TUN_MTU="${TUN_MTU:-1500}"
TUN_ADDR4="${TUN_ADDR4:-198.18.0.1}"
TUN_ADDR6="${TUN_ADDR6:-}"

info "拉取依赖 (go mod tidy) ..."
go mod tidy

info "编译 ${BIN_NAME} ..."
go build -o "${BIN_NAME}" .

info "启动 tun 模式：${SERVER_HOST}:${SERVER_PORT}${WS_PATH} -> 网卡 ${TUN_NAME} (${TUN_ADDR4})"
echo ""

# 用一个数组拼参数，方便按需追加 -tun-addr6
args=(
  -host "${SERVER_HOST}"
  -port "${SERVER_PORT}"
  -path "${WS_PATH}"
  -uuid "${UUID}"
  -token "${TOKEN:-}"
  -tls="${USE_TLS}"
  -insecure="${INSECURE}"
  -log-level "${LOG_LEVEL}"
  -tun
  -tun-name "${TUN_NAME}"
  -tun-mtu "${TUN_MTU}"
  -tun-addr4 "${TUN_ADDR4}"
)
[ -n "${TUN_ADDR6}" ] && args+=(-tun-addr6 "${TUN_ADDR6}")

# 后台启动，拿到 PID 才能在配完路由前打印提示；trap 保证 Ctrl+C 时一起退出
./"${BIN_NAME}" "${args[@]}" &
PID=$!
trap 'kill "$PID" 2>/dev/null || true' EXIT INT TERM

sleep 1
if ! kill -0 "$PID" 2>/dev/null; then
  error "客户端启动失败，看看上面的报错（常见原因：网卡名格式不对，或者服务器连不上）"
  exit 1
fi

warn "网卡已创建，但系统流量还不会自动经过它，需要手动配置路由（否则跟没开一样）："
warn ""
warn "  Linux 示例："
warn "    sudo ip link set ${TUN_NAME} up"
warn "    sudo ip addr add ${TUN_ADDR4}/15 dev ${TUN_NAME}"
warn "    SERVER_IP=\$(dig +short ${SERVER_HOST} | head -1)"
warn "    ORIG_GW=\$(ip route show default | awk '{print \$3; exit}')"
warn "    ORIG_DEV=\$(ip route show default | awk '{print \$5; exit}')"
warn "    sudo ip route add \$SERVER_IP via \$ORIG_GW dev \$ORIG_DEV   # 防止死循环，必须先加"
warn "    sudo ip route add 0.0.0.0/1 dev ${TUN_NAME}"
warn "    sudo ip route add 128.0.0.0/1 dev ${TUN_NAME}"
warn ""
warn "  macOS/Windows 命令见 TUN-SETUP.md。"
warn ""
info "按 Ctrl+C 停止（会自动把上面创建的进程一起结束，但已加的路由不会自动撤销）"

wait "$PID"
