package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

// prefixConn 包装一个 net.Conn，在其之前插入一段已经读到内存中的 "预读" 数据，
// 使调用方可以像没有发生过预读一样正常 Read。用于消费完 VLESS 响应头之后，
// 把响应头后面紧跟的应用层数据（如果服务端在同一帧里带了）还给上层。
type prefixConn struct {
	net.Conn
	prefix []byte
}

func (c *prefixConn) Read(p []byte) (int, error) {
	if len(c.prefix) > 0 {
		n := copy(p, c.prefix)
		c.prefix = c.prefix[n:]
		return n, nil
	}
	return c.Conn.Read(p)
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

// dialOnce 尝试建立一次 VLESS/WS 隧道，不做任何重试。
func dialOnce(ctx context.Context, cfg *Config, uuid [16]byte, targetAddr string, targetPort uint16) (net.Conn, error) {
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

	hdr, err := buildVlessHeader(uuid, targetAddr, targetPort)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("build vless header: %w", err)
	}
	if _, err := nc.Write(hdr); err != nil {
		nc.Close()
		return nil, fmt.Errorf("write vless header: %w", err)
	}

	// 读取服务端 VLESS 响应头：ver(1) + addonsLen(1) + addons(addonsLen)
	respBuf := make([]byte, 0, 64)
	readBuf := make([]byte, 512)
	for {
		n, err := nc.Read(readBuf)
		if n > 0 {
			respBuf = append(respBuf, readBuf[:n]...)
		}
		if err != nil {
			nc.Close()
			return nil, fmt.Errorf("read vless response: %w", err)
		}
		_, headerLen, perr := parseVlessRespHeader(respBuf)
		if perr != nil {
			if len(respBuf) < 2 || len(respBuf) < headerLen {
				continue // 数据不足，继续读
			}
			nc.Close()
			return nil, fmt.Errorf("parse vless response: %w", perr)
		}
		leftover := respBuf[headerLen:]
		return &prefixConn{Conn: nc, prefix: leftover}, nil
	}
}

// DialVless 连接到 VLESS-WS 服务端，完成 VLESS 握手，
// 返回一个可以直接 io.Copy 的 net.Conn，读写的都是目标地址的原始 TCP 数据。
//
// 建立隧道失败时会按指数退避重试 cfg.ConnectRetries 次（不含首次尝试），
// 用来吸收网络抖动、握手瞬时失败等一次性问题，减少偶发失败导致的整条
// 本地代理连接被中断。重试仅发生在隧道建立阶段，一旦隧道建立成功、
// 数据开始转发后中途断开不会在这里自动重连（VLESS 协议没有会话恢复能力，
// 强行重连只是换一条新隧道连到同一目标，对已经在传输中的数据流没有意义，
// 交给上层应用自己重试更安全）。
func DialVless(ctx context.Context, cfg *Config, log *Logger, uuid [16]byte, targetAddr string, targetPort uint16) (net.Conn, error) {
	var lastErr error
	attempts := cfg.ConnectRetries + 1
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(cfg.RetryBaseMs) * time.Duration(1<<uint(attempt-1)) * time.Millisecond
			backoff += time.Duration(rand.Intn(100)) * time.Millisecond // 加一点抖动，避免重试风暴
			log.Debug(fmt.Sprintf("retry %d/%d for %s:%d in %s", attempt, cfg.ConnectRetries, targetAddr, targetPort, backoff))
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}

		conn, err := dialOnce(ctx, cfg, uuid, targetAddr, targetPort)
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}
