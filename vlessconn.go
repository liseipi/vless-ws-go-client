package main

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"time"
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

// dialOnce 在共享的 WS+yamux 会话上开一个新 stream 并完成一次 VLESS 握手，
// 不做任何重试。stream 本身的建立通常不需要新的 TCP/TLS/WS 握手（除非
// 当前没有可用会话），代价很低。
func dialOnce(ctx context.Context, sm *sessionManager, uuid [16]byte, cmd byte, targetAddr string, targetPort uint16) (net.Conn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, time.Duration(sm.cfg.DialTimeoutMS)*time.Millisecond)
	defer cancel()

	stream, err := sm.openStream(dialCtx)
	if err != nil {
		return nil, fmt.Errorf("open stream: %w", err)
	}

	hdr, err := buildVlessHeader(uuid, cmd, targetAddr, targetPort)
	if err != nil {
		stream.Close()
		return nil, fmt.Errorf("build vless header: %w", err)
	}
	if _, err := stream.Write(hdr); err != nil {
		stream.Close()
		return nil, fmt.Errorf("write vless header: %w", err)
	}

	// 读取服务端 VLESS 响应头：ver(1) + addonsLen(1) + addons(addonsLen)
	respBuf := make([]byte, 0, 64)
	readBuf := make([]byte, 512)
	for {
		n, err := stream.Read(readBuf)
		if n > 0 {
			respBuf = append(respBuf, readBuf[:n]...)
		}
		if err != nil {
			stream.Close()
			return nil, fmt.Errorf("read vless response: %w", err)
		}
		_, headerLen, perr := parseVlessRespHeader(respBuf)
		if perr != nil {
			if len(respBuf) < 2 || len(respBuf) < headerLen {
				continue // 数据不足，继续读
			}
			stream.Close()
			return nil, fmt.Errorf("parse vless response: %w", perr)
		}
		leftover := respBuf[headerLen:]
		return &prefixConn{Conn: stream, prefix: leftover}, nil
	}
}

// DialVless 连接到 VLESS-WS 服务端，完成 VLESS 握手（cmd=TCP），
// 返回一个可以直接 io.Copy 的 net.Conn，读写的都是目标地址的原始 TCP 数据。
//
// 建立隧道失败时会按指数退避重试 cfg.ConnectRetries 次（不含首次尝试），
// 用来吸收网络抖动、握手瞬时失败等一次性问题，减少偶发失败导致的整条
// 本地代理连接被中断。重试仅发生在隧道建立阶段，一旦隧道建立成功、
// 数据开始转发后中途断开不会在这里自动重连（VLESS 协议没有会话恢复能力，
// 强行重连只是换一条新隧道连到同一目标，对已经在传输中的数据流没有意义，
// 交给上层应用自己重试更安全）。
func DialVless(ctx context.Context, sm *sessionManager, log *Logger, uuid [16]byte, targetAddr string, targetPort uint16) (net.Conn, error) {
	return dialVlessWithRetry(ctx, sm, log, uuid, cmdTCP, targetAddr, targetPort)
}

// DialVlessUDP 跟 DialVless 逻辑一致，只是 cmd=UDP：服务端收到后不会建 TCP
// 连接，而是建一个指向 targetAddr:targetPort 的 UDP socket。返回的 net.Conn
// 上读写的不是原始字节流，而是长度前缀帧（2 字节大端长度 + UDP 包内容），
// 一个 net.Conn 对应一个固定目标地址。
func DialVlessUDP(ctx context.Context, sm *sessionManager, log *Logger, uuid [16]byte, targetAddr string, targetPort uint16) (net.Conn, error) {
	return dialVlessWithRetry(ctx, sm, log, uuid, cmdUDP, targetAddr, targetPort)
}

func dialVlessWithRetry(ctx context.Context, sm *sessionManager, log *Logger, uuid [16]byte, cmd byte, targetAddr string, targetPort uint16) (net.Conn, error) {
	var lastErr error
	attempts := sm.cfg.ConnectRetries + 1
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(sm.cfg.RetryBaseMs) * time.Duration(1<<uint(attempt-1)) * time.Millisecond
			backoff += time.Duration(rand.Intn(100)) * time.Millisecond // 加一点抖动，避免重试风暴
			log.Debug(fmt.Sprintf("retry %d/%d for %s:%d in %s", attempt, sm.cfg.ConnectRetries, targetAddr, targetPort, backoff))
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}

		conn, err := dialOnce(ctx, sm, uuid, cmd, targetAddr, targetPort)
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}
