package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
)

// Config 保存客户端全部运行时配置。
// 支持命令行参数覆盖环境变量，环境变量覆盖默认值。
type Config struct {
	ServerHost string // 服务器地址（域名或 IP，用于实际 TCP 拨号），不带协议前缀，例如 example.com 或 1.2.3.4
	ServerPort string // 服务器端口，例如 443、8081
	WSPath     string // 连接路径，例如 /api
	UseTLS     bool   // true 用 wss://，false 用 ws://（生产环境务必为 true）

	// SNI / WSHost 默认等于 ServerHost，一般不需要单独设置。
	// 只有在"实际拨号地址"和"TLS SNI / HTTP Host 头"需要不一致时才用得上，
	// 例如直连服务器 IP 但仍要按域名做 TLS/虚拟主机路由（类似 CDN 场景）。
	SNI    string // TLS ClientHello 里的 server_name，留空则等于 ServerHost
	WSHost string // HTTP 握手请求里的 Host 头，留空则等于 ServerHost

	LocalIP   string // 本地监听 IP，例如 127.0.0.1（只在本机使用）或 0.0.0.0（局域网共享，请谨慎）
	LocalPort string // 本地监听端口，SOCKS5 与 HTTP(S) 代理共用同一端口，自动识别协议

	UUID             string
	Token            string
	DialTimeoutMS    int64
	ConnectRetries   int   // 建立 VLESS/WS 隧道失败时的额外重试次数（不含首次尝试）
	RetryBaseMs      int64 // 重试退避基准时间（毫秒），指数增长：base, base*2, base*4...
	KeepAliveSec     int   // TCP KeepAlive 探测间隔（秒），0 表示关闭
	Insecure         bool  // 跳过 TLS 证书校验（自签名证书时使用，生产环境不建议）
	LogLevel         string
	YamuxWindowBytes uint32 // 单个 yamux stream 的接收窗口大小，见 sessionpool.go 里的说明
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	switch v {
	case "1", "true", "TRUE", "yes":
		return true
	case "0", "false", "FALSE", "no":
		return false
	default:
		return def
	}
}

func envInt64(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func LoadConfig() *Config {
	cfg := &Config{}

	flag.StringVar(&cfg.ServerHost, "host", envStr("SERVER_HOST", ""), "服务器地址（域名或 IP），不要带 ws:// 或 wss://，例如 example.com")
	flag.StringVar(&cfg.ServerPort, "port", envStr("SERVER_PORT", "443"), "服务器端口，例如 443")
	flag.StringVar(&cfg.WSPath, "path", envStr("WS_PATH", "/api"), "连接路径，需与服务端 WS_PATH 一致，例如 /api")
	flag.BoolVar(&cfg.UseTLS, "tls", envBool("USE_TLS", true), "是否使用 TLS（wss://）。裸端口直连测试时可设为 false")

	flag.StringVar(&cfg.SNI, "sni", envStr("SNI", ""), "TLS SNI，留空默认等于 -host")
	flag.StringVar(&cfg.WSHost, "ws-host", envStr("WS_HOST", ""), "HTTP 握手 Host 头，留空默认等于 -host")

	flag.StringVar(&cfg.LocalIP, "local-ip", envStr("LOCAL_IP", "127.0.0.1"), "本地监听 IP，仅本机使用请保持 127.0.0.1")
	flag.StringVar(&cfg.LocalPort, "local-port", envStr("LOCAL_PORT", "1080"), "本地监听端口，SOCKS5 与 HTTP(S) 代理共用此端口")

	flag.StringVar(&cfg.UUID, "uuid", envStr("UUID", ""), "VLESS UUID，需与服务端一致")
	flag.StringVar(&cfg.Token, "token", envStr("TOKEN", ""), "鉴权 Token（若服务端启用了 TOKEN）")
	flag.Int64Var(&cfg.DialTimeoutMS, "dial-timeout-ms", 10000, "单次连接服务端超时（毫秒）")
	flag.IntVar(&cfg.ConnectRetries, "connect-retries", 2, "建立隧道失败时的额外重试次数（不含首次尝试），0 表示不重试")
	flag.Int64Var(&cfg.RetryBaseMs, "retry-base-ms", 300, "重试退避基准时间（毫秒），指数增长")
	flag.IntVar(&cfg.KeepAliveSec, "keepalive-sec", 30, "TCP KeepAlive 探测间隔（秒），0 表示关闭")
	flag.BoolVar(&cfg.Insecure, "insecure", envBool("INSECURE", false), "跳过 TLS 证书校验（自签名证书调试用）")
	flag.StringVar(&cfg.LogLevel, "log-level", envStr("LOG_LEVEL", "info"), "日志级别：debug/info/warn/error")
	var yamuxWindowKB int64
	flag.Int64Var(&yamuxWindowKB, "yamux-window-kb", int64(envInt64("YAMUX_WINDOW_KB", 16*1024)), "单个逻辑连接的 yamux 接收窗口大小（KB），需与服务端 YAMUX_WINDOW_KB 保持数量级一致；调大能提升大文件传输吞吐，但会增加内存占用")
	flag.Parse()
	cfg.YamuxWindowBytes = uint32(yamuxWindowKB) * 1024

	if cfg.ServerHost == "" || cfg.UUID == "" {
		fmt.Fprintln(os.Stderr, "错误: 必须提供 -host 和 -uuid（或环境变量 SERVER_HOST / UUID）")
		flag.Usage()
		os.Exit(1)
	}

	return cfg
}

// EffectiveSNI 返回实际用于 TLS server_name 的值，未单独设置时回退到 ServerHost。
func (c *Config) EffectiveSNI() string {
	if c.SNI != "" {
		return c.SNI
	}
	return c.ServerHost
}

// EffectiveWSHost 返回实际用于 HTTP 握手 Host 头的值，未单独设置时回退到 ServerHost。
func (c *Config) EffectiveWSHost() string {
	if c.WSHost != "" {
		return c.WSHost
	}
	return c.ServerHost
}

// ServerURL 拼出用于展示/日志的服务器地址，例如 wss://example.com:443/api。
// 注意：实际拨号目标由 DialAddr() 决定，Host 头/SNI 由 EffectiveWSHost()/EffectiveSNI() 决定，
// 三者在默认配置下完全一致；只有显式设置了 -sni/-ws-host 时才会分开。
func (c *Config) ServerURL() string {
	scheme := "ws"
	if c.UseTLS {
		scheme = "wss"
	}
	return fmt.Sprintf("%s://%s/%s", scheme, net.JoinHostPort(c.ServerHost, c.ServerPort), trimLeadingSlash(c.WSPath))
}

// DialURL 是真正传给 WebSocket 客户端库的 URL：host 部分用 EffectiveWSHost()，
// 这样库自动生成的 Host 头和 TLS SNI 默认就是期望的值；真正的 TCP 目标地址
// 由 vlessconn.go 里自定义的 DialContext 固定指向 ServerHost:ServerPort。
func (c *Config) DialURL() string {
	scheme := "ws"
	if c.UseTLS {
		scheme = "wss"
	}
	return fmt.Sprintf("%s://%s/%s", scheme, net.JoinHostPort(c.EffectiveWSHost(), c.ServerPort), trimLeadingSlash(c.WSPath))
}

// ListenAddr 拼出本地代理监听地址，例如 127.0.0.1:1080
func (c *Config) ListenAddr() string {
	return net.JoinHostPort(c.LocalIP, c.LocalPort)
}

func trimLeadingSlash(p string) string {
	for len(p) > 0 && p[0] == '/' {
		p = p[1:]
	}
	return p
}
