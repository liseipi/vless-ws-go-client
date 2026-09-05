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

	// 【修复】给这次 VLESS 握手（写请求头 + 等响应头）设置一个明确的截止
	// 时间。之前这里完全没有超时：dialCtx 只约束了上面 openStream 这一步，
	// 一旦服务端那边目标连接卡住（黑洞网络/防火墙静默丢包不回 RST），下面
	// 的 stream.Read 会无限期阻塞，跟 -dial-timeout-ms 配置的值完全脱节，
	// 用户体感就是"卡住/连不上"，还会让这个 goroutine 和这个 stream 一直
	// 占用资源不释放。yamux.Stream 实现了 net.Conn，支持 SetDeadline。
	if dl, ok := dialCtx.Deadline(); ok {
		_ = stream.SetDeadline(dl)
	}
	// 握手成功后必须清掉这个超时，否则会误伤后续正常的数据转发阶段
	// （比如大文件下载耗时超过握手阶段的 DialTimeoutMS）。
	handshakeOK := false
	defer func() {
		if handshakeOK {
			_ = stream.SetDeadline(time.Time{})
		}
	}()

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
		handshakeOK = true
		return &prefixConn{Conn: stream, prefix: leftover}, nil
	}
}

// dialVlessLegacyOnce 走一次全新的 WS 连接，不经过 yamux，直接在这条连接上
// 做一次 VLESS 握手——这是兼容标准 VLESS-WS 服务端（不认识本项目的 yamux
// 封装）的路径，每次调用都要重新完整握手一遍，没有连接复用带来的性能提升，
// 但保证能连上任意标准实现的 VLESS-WS 服务端。
func dialVlessLegacyOnce(ctx context.Context, cfg *Config, uuid [16]byte, cmd byte, targetAddr string, targetPort uint16) (net.Conn, error) {
	wsConn, err := dialLegacyWS(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("legacy dial: %w", err)
	}

	hdr, err := buildVlessHeader(uuid, cmd, targetAddr, targetPort)
	if err != nil {
		wsConn.Close()
		return nil, fmt.Errorf("build vless header: %w", err)
	}
	if _, err := wsConn.Write(hdr); err != nil {
		wsConn.Close()
		return nil, fmt.Errorf("write vless header: %w", err)
	}

	respBuf := make([]byte, 0, 64)
	readBuf := make([]byte, 512)
	for {
		n, err := wsConn.Read(readBuf)
		if n > 0 {
			respBuf = append(respBuf, readBuf[:n]...)
		}
		if err != nil {
			wsConn.Close()
			return nil, fmt.Errorf("read vless response: %w", err)
		}
		_, headerLen, perr := parseVlessRespHeader(respBuf)
		if perr != nil {
			if len(respBuf) < 2 || len(respBuf) < headerLen {
				continue
			}
			wsConn.Close()
			return nil, fmt.Errorf("parse vless response: %w", perr)
		}
		leftover := respBuf[headerLen:]
		return &prefixConn{Conn: wsConn, prefix: leftover}, nil
	}
}

func dialVlessLegacyWithRetry(ctx context.Context, cfg *Config, log *Logger, uuid [16]byte, cmd byte, targetAddr string, targetPort uint16) (net.Conn, error) {
	var lastErr error
	attempts := cfg.ConnectRetries + 1
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(cfg.RetryBaseMs) * time.Duration(1<<uint(attempt-1)) * time.Millisecond
			backoff += time.Duration(rand.Intn(100)) * time.Millisecond
			log.Debug(fmt.Sprintf("[legacy] retry %d/%d for %s:%d in %s", attempt, cfg.ConnectRetries, targetAddr, targetPort, backoff))
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}
		conn, err := dialVlessLegacyOnce(ctx, cfg, uuid, cmd, targetAddr, targetPort)
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// dialWithProtocolDetection 是 DialVless/DialVlessUDP 的实际入口，负责在
// modeYamux（服务端支持本项目的连接复用协议）和 modeLegacy（标准 VLESS-WS
// 服务端，比如 Cloudflare 后面常见的 Xray-core 原生部署）之间自动选择：
//
//   - 已经确定模式（手动指定，或前面某次请求探测/用过之后记住的结果）时，
//     直接走对应路径，没有额外开销；
//   - 第一次遇到某个服务端（modeUnknown）时，用较短的探测超时尝试一次
//     yamux 握手：如果能拿到有效的 VLESS 响应，说明服务端认识这层协议，
//     记住 modeYamux，之后都走连接复用这条快路径；如果超时或者响应不对，
//     说明这是个标准服务端，记住 modeLegacy，并立刻改用兼容路径把这次
//     请求真正接通，整个过程对上层完全透明，不需要用户手动配置。
//   - 用 probeMu 保证同一时刻只有一个 goroutine 在探测，程序刚启动时如果
//     有多个并发请求同时打进来，只有第一个会触发探测，其余的等它探测完
//     直接复用结果，不会重复探测。
func dialWithProtocolDetection(ctx context.Context, sm *sessionManager, log *Logger, uuid [16]byte, cmd byte, targetAddr string, targetPort uint16) (net.Conn, error) {
	switch sm.getMode() {
	case modeLegacy:
		return dialVlessLegacyWithRetry(ctx, sm.cfg, log, uuid, cmd, targetAddr, targetPort)
	case modeYamux:
		return dialVlessWithRetry(ctx, sm, log, uuid, cmd, targetAddr, targetPort)
	}

	// modeUnknown：需要探测，加锁保证只有一个 goroutine 真正探测
	sm.probeMu.Lock()
	defer sm.probeMu.Unlock()

	// 等锁的这段时间里，可能已经有另一个 goroutine 把模式探测出来了，
	// 直接按探测结果走对应路径，不用再重新探测一次。
	switch sm.getMode() {
	case modeLegacy:
		return dialVlessLegacyWithRetry(ctx, sm.cfg, log, uuid, cmd, targetAddr, targetPort)
	case modeYamux:
		return dialVlessWithRetry(ctx, sm, log, uuid, cmd, targetAddr, targetPort)
	}

	log.Info("首次连接该服务端，探测是否支持连接复用协议...")
	probeCtx, cancel := context.WithTimeout(ctx, time.Duration(sm.cfg.ProbeTimeoutMS)*time.Millisecond)
	conn, err := dialOnce(probeCtx, sm, uuid, cmd, targetAddr, targetPort)
	cancel()
	if err == nil {
		sm.setModeAndCleanup(modeYamux)
		log.Info("探测成功：服务端支持连接复用协议，后续请求将复用同一条底层连接")
		return conn, nil
	}

	// 这里不能一看到探测失败就直接判定为 legacy——"没拿到 VLESS 响应"
	// 至少有三种完全不同的原因，必须严格区分：
	//
	//   1.【纯网络问题，跟协议完全无关】探测这一次的 TCP 连接 / TLS 握手 /
	//      WS Upgrade 本身就没成功（DNS 抖动、连接超时、TLS 握手失败、
	//      中间设备瞬时丢包……）。这种情况下 sm.current 根本没被赋值过
	//      （dialSession 只有握手成功才会设置它），如果误判成 legacy 并
	//      永久缓存下去，而服务端其实完全支持 yamux 只是不支持 legacy，
	//      就会导致【这个客户端进程之后所有请求永久失败】——这正是
	//      "网络正常、服务器正常，但偶尔连不上"的根本原因，必须原样把
	//      这次网络错误返回、不缓存任何结论，让下一次请求重新探测。
	//   2.【服务端认识 yamux，只是这次目标偶发不通】WS+yamux 会话已经
	//      建立成功，只是这次探测请求要连的目标恰好连不上（DNS 失败、
	//      目标拒绝连接等），服务端按 VLESS 协议正常行为只关闭这一个
	//      stream，底层会话本身完好无损——判定为 yamux。
	//   3.【真正的协议不兼容】WS 已经建立成功，但对端把 yamux 帧当成
	//      别的东西解析失败，直接把整条 WS 连接掐断，导致底层会话本身
	//      也跟着挂掉——只有这种情况才能判定为 legacy。
	sm.mu.Lock()
	sess := sm.current
	sm.mu.Unlock()

	if sess == nil {
		// 对应情况 1：连 WS 都没建立成功，保持 modeUnknown，不缓存任何
		// 结论，直接把这次网络错误原样返回给调用方。
		log.Debug(fmt.Sprintf("探测阶段底层 WS 连接未建立成功（%s），这是网络问题而非协议不兼容，保持自动探测状态，下次请求会重新尝试", err.Error()))
		return nil, err
	}

	if !sess.IsClosed() {
		// 对应情况 2
		sm.setModeAndCleanup(modeYamux)
		log.Debug(fmt.Sprintf("探测请求本身失败（%s），但底层会话仍然存活，判定服务端支持连接复用协议，仅这次目标连接失败", err.Error()))
		return nil, err
	}

	// 对应情况 3
	log.Warn(fmt.Sprintf("连接复用协议探测失败且底层连接被对端整体断开（%s），判定为标准 VLESS-WS 服务端，自动切换为兼容模式", err.Error()))
	sm.setModeAndCleanup(modeLegacy)
	return dialVlessLegacyWithRetry(ctx, sm.cfg, log, uuid, cmd, targetAddr, targetPort)
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
	return dialWithProtocolDetection(ctx, sm, log, uuid, cmdTCP, targetAddr, targetPort)
}

// DialVlessUDP 跟 DialVless 逻辑一致，只是 cmd=UDP：服务端收到后不会建 TCP
// 连接，而是建一个指向 targetAddr:targetPort 的 UDP socket。返回的 net.Conn
// 上读写的不是原始字节流，而是长度前缀帧（2 字节大端长度 + UDP 包内容），
// 一个 net.Conn 对应一个固定目标地址。
func DialVlessUDP(ctx context.Context, sm *sessionManager, log *Logger, uuid [16]byte, targetAddr string, targetPort uint16) (net.Conn, error) {
	return dialWithProtocolDetection(ctx, sm, log, uuid, cmdUDP, targetAddr, targetPort)
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
