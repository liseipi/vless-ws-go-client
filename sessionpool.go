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
//   - 域名同时有 A/AAAA 记录时，用 dialPreferIPv6 代替 Go 默认的 Happy
//     Eyeballs，见该函数注释里的说明。
func buildHTTPClient(cfg *Config) *http.Client {
	dialer := &net.Dialer{
		Timeout:   time.Duration(cfg.DialTimeoutMS) * time.Millisecond,
		KeepAlive: time.Duration(cfg.KeepAliveSec) * time.Second,
	}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			if !cfg.PreferIPv6 {
				return dialer.DialContext(ctx, network, net.JoinHostPort(cfg.ServerHost, cfg.ServerPort))
			}
			delay := time.Duration(cfg.IPv4FallbackDelayMS) * time.Millisecond
			return dialPreferIPv6(ctx, dialer, cfg.ServerHost, cfg.ServerPort, delay)
		},
		TLSClientConfig: &tls.Config{
			ServerName:         cfg.EffectiveSNI(),
			InsecureSkipVerify: cfg.Insecure,
		},
	}
	return &http.Client{Transport: transport}
}

// dialPreferIPv6 针对"域名同时有 IPv4/IPv6 记录、但两条路径质量差异很大"
// 的场景，实现一个明确偏向 IPv6 的拨号策略：
//
//   - IPv6 立即开始拨号；
//   - IPv4 默认延迟 ipv4FallbackDelay（比如 100ms）之后才作为"兜底"启动，
//     如果 IPv6 在这之前已经失败，则不用等满这个延迟，立刻启动 IPv4；
//   - 两边谁先握手成功就用谁，另一边仍在进行中的拨号会被取消。
//
// 这跟 Go net.Dialer 内置的 Happy Eyeballs（RFC 6555 Fast Fallback）不是一回事：
// Go 默认实现只关心"谁的 TCP 握手先完成"，两个地址族的拨号几乎同时开始，
// 完全不偏向哪一边。但握手成功不代表这条路径质量好——比如某些 CDN/网络
// 环境下，IPv4 路径握手（SYN/SYN-ACK）经常能很快通过，只是后续实际数据
// 传输被限速或不稳定；这种情况下 Go 的默认策略会经常"赌"到握手快但传输
// 差的 IPv4 上。显式让 IPv6 抢跑、IPv4 仅作延迟兜底，能大幅降低落到差
// 路径上的概率，同时保留"IPv6 真的不通时能尽快切回 IPv4"的兜底能力。
//
// 如果域名解析出来只有一种地址族，直接拨那个族，不存在"比较"的问题。
func dialPreferIPv6(ctx context.Context, dialer *net.Dialer, host, port string, ipv4FallbackDelay time.Duration) (net.Conn, error) {
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}

	var v6addrs, v4addrs []net.IPAddr
	for _, ip := range ips {
		if ip.IP.To4() == nil {
			v6addrs = append(v6addrs, ip)
		} else {
			v4addrs = append(v4addrs, ip)
		}
	}

	switch {
	case len(v6addrs) == 0 && len(v4addrs) == 0:
		return nil, fmt.Errorf("resolve %s: no A/AAAA records", host)
	case len(v6addrs) == 0:
		return dialer.DialContext(ctx, "tcp4", net.JoinHostPort(v4addrs[0].IP.String(), port))
	case len(v4addrs) == 0:
		return dialer.DialContext(ctx, "tcp6", net.JoinHostPort(v6addrs[0].IP.String(), port))
	}

	// 双栈都有：v6 立即拨号；v4 延迟 ipv4FallbackDelay 后兜底启动，
	// v6 提前失败时不用等满延迟，立刻触发 v4。谁先连通用谁，函数返回时
	// 通过取消 raceCtx 让另一路还没完成的拨号自行终止。
	type result struct {
		conn net.Conn
		err  error
	}

	raceCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	v6FailedCh := make(chan struct{})
	resCh := make(chan result, 2)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		c, err := dialer.DialContext(raceCtx, "tcp6", net.JoinHostPort(v6addrs[0].IP.String(), port))
		if err != nil {
			close(v6FailedCh)
		}
		resCh <- result{c, err}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case <-raceCtx.Done():
			resCh <- result{nil, raceCtx.Err()}
			return
		case <-v6FailedCh:
			// v6 已经先失败了，不用再等延迟，立刻启动 v4 兜底
		case <-time.After(ipv4FallbackDelay):
		}
		c, err := dialer.DialContext(raceCtx, "tcp4", net.JoinHostPort(v4addrs[0].IP.String(), port))
		resCh <- result{c, err}
	}()

	go func() {
		wg.Wait()
		close(resCh)
	}()

	var firstErr error
	for r := range resCh {
		if r.err == nil {
			return r.conn, nil
		}
		if firstErr == nil {
			firstErr = r.err
		}
	}
	return nil, firstErr
}
