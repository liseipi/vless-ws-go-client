package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
)

// sessionManager 维护一条到服务端的底层 WS+yamux 会话。
//
// 之前的实现里，每一次 SOCKS5 CONNECT / HTTP CONNECT / UDP 目标都会触发一次
// 完整的 TCP 拨号 + TLS 握手 + WS Upgrade + VLESS 握手；本地代理转发的网页
// 一多，握手开销就很明显。sessionManager 把"建立底层连接"和"开一条逻辑
// 连接"拆成了两件事：
//   - 底层连接：一条 WS 连接上跑一个 yamux 会话，只有在没有可用会话时才会
//     真正走一次完整握手；
//   - 逻辑连接：每次上层需要转发一个新的 TCP/UDP 目标时，直接在已有会话上
//     open 一个新的 yamux stream，代价接近于零（不需要新的 TCP/TLS/WS）。
//
// 一个 stream 出错或被关闭只影响这一路转发，不会影响同一会话上的其它请求，
// 也不会导致底层 WS 连接被误关闭。
type sessionManager struct {
	cfg *Config
	log *Logger

	mu      sync.Mutex
	current *yamux.Session
}

func newSessionManager(cfg *Config, log *Logger) *sessionManager {
	return &sessionManager{cfg: cfg, log: log}
}

// openStream 返回一个可用于收发一次 VLESS 请求的 stream：
// 优先复用现有会话；会话不存在、已关闭，或者在其上开流失败
// （例如对端刚好断开）时，才会重新走一次 WS+yamux 握手。
func (m *sessionManager) openStream(ctx context.Context) (net.Conn, error) {
	m.mu.Lock()
	sess := m.current
	m.mu.Unlock()

	if sess != nil && !sess.IsClosed() {
		stream, err := sess.OpenStream()
		if err == nil {
			return stream, nil
		}
		m.log.Debug(fmt.Sprintf("open stream on existing session failed (%s), redialing", err.Error()))
	}

	newSess, err := m.dialSession(ctx)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	// 简化并发处理：直接覆盖为最新会话。极端并发下可能短暂多建 1~2 条
	// 会话，旧会话不会再被取用，之后随进程正常关闭；如需严格单例可在这里
	// 判断 m.current 是否已被其它 goroutine 抢先替换后 Close 掉多余的一条。
	m.current = newSess
	m.mu.Unlock()

	return newSess.OpenStream()
}

// dialSession 走一次完整的 TCP + TLS + WS Upgrade 握手，并在其上建立
// yamux 客户端会话。只有在没有可复用会话时才会被调用。
func (m *sessionManager) dialSession(ctx context.Context) (*yamux.Session, error) {
	cfg := m.cfg
	dialCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.DialTimeoutMS)*time.Millisecond)
	defer cancel()

	httpClient := buildHTTPClient(cfg)
	header := make(http.Header)
	if cfg.Token != "" {
		header.Set("X-Auth-Token", cfg.Token)
	}
	opts := &websocket.DialOptions{
		HTTPClient: httpClient,
		HTTPHeader: header,
	}

	wsConn, _, err := websocket.Dial(dialCtx, cfg.DialURL(), opts)
	if err != nil {
		return nil, fmt.Errorf("websocket dial: %w", err)
	}
	wsConn.SetReadLimit(-1)

	// 用 background ctx（而非 dialCtx）构造长连接的 net.Conn，
	// 否则 dialCtx 超时后底层连接会被取消关闭。
	nc := websocket.NetConn(context.Background(), wsConn, websocket.MessageBinary)

	ymCfg := yamux.DefaultConfig()
	ymCfg.LogOutput = io.Discard // 用自己的 Logger，屏蔽 yamux 自带的标准库日志输出
	ymCfg.EnableKeepAlive = true
	if ka := time.Duration(cfg.KeepAliveSec) * time.Second; ka > 0 {
		ymCfg.KeepAliveInterval = ka
	}
	ymCfg.ConnectionWriteTimeout = 15 * time.Second
	// yamux 默认单流窗口只有 256KB，会严重限制单个 stream（比如一次大文件
	// 下载/上传）的吞吐量：发送方发满 256KB 未确认数据就必须停下来等对端
	// 的窗口更新包，相当于每 256KB 就插入一次往返等待，RTT 越高影响越大。
	// 这里换成更大的窗口（默认 16MB，可用 -yamux-window-kb 调整），
	// 需要和服务端的 YAMUX_WINDOW_KB 保持同一数量级，两边独立配置，
	// 不要求完全相等（yamux 两端各自声明自己的接收窗口）。
	if cfg.YamuxWindowBytes > 0 {
		ymCfg.MaxStreamWindowSize = cfg.YamuxWindowBytes
	}

	sess, err := yamux.Client(nc, ymCfg)
	if err != nil {
		_ = nc.Close()
		return nil, fmt.Errorf("yamux client: %w", err)
	}
	m.log.Debug("new WS+yamux session established")
	return sess, nil
}

// buildHTTPClient 构造用于 WebSocket 握手的 http.Client：
//   - DialContext 固定拨号到 cfg.ServerHost:cfg.ServerPort，忽略 addr 参数里的 host。
//     这样 TCP 连接目标（ServerHost）和 TLS SNI / HTTP Host 头（EffectiveSNI/EffectiveWSHost）
//     就彻底解耦了：可以直连 IP，同时仍然发送期望的 SNI/Host，默认配置下三者一致，行为不变。
//   - 显式开启 TCP KeepAlive，长时间空闲的隧道不容易被 NAT/防火墙判定为死连接而丢弃。
func buildHTTPClient(cfg *Config) *http.Client {
	dialer := &net.Dialer{
		Timeout:   time.Duration(cfg.DialTimeoutMS) * time.Millisecond,
		KeepAlive: time.Duration(cfg.KeepAliveSec) * time.Second,
	}
	realAddr := net.JoinHostPort(cfg.ServerHost, cfg.ServerPort)

	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, realAddr)
		},
		TLSClientConfig: &tls.Config{
			ServerName:         cfg.EffectiveSNI(),
			InsecureSkipVerify: cfg.Insecure,
		},
	}
	return &http.Client{Transport: transport}
}
