package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"time"
)

const (
	socksVer5        byte = 0x05
	socksCmdConn     byte = 0x01
	socksCmdUDPAssoc byte = 0x03
	socksAtypIPv4    byte = 0x01
	socksAtypDom     byte = 0x03
	socksAtypIPv6    byte = 0x04
	socksRepOK       byte = 0x00
	socksRepFail     byte = 0x01
	socksRepCmdErr   byte = 0x07
)

// ProxyServer 在同一个本地端口上同时提供 SOCKS5 和 HTTP(S) 代理服务，
// 通过窥探连接的第一个字节自动区分协议：
//   - 0x05          -> SOCKS5
//   - 其余（ASCII）  -> HTTP 请求行（CONNECT 或普通 GET/POST 等）
type ProxyServer struct {
	cfg  *Config
	log  *Logger
	uuid [16]byte
}

func NewProxyServer(cfg *Config, log *Logger, uuid [16]byte) *ProxyServer {
	return &ProxyServer{cfg: cfg, log: log, uuid: uuid}
}

func newConnID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *ProxyServer) ListenAndServe() error {
	ln, err := net.Listen("tcp", s.cfg.ListenAddr())
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.cfg.ListenAddr(), err)
	}
	s.log.Info(fmt.Sprintf("SOCKS5/HTTP proxy listening on %s -> %s", s.cfg.ListenAddr(), s.cfg.ServerURL()))

	for {
		conn, err := ln.Accept()
		if err != nil {
			s.log.Warn("accept:", err.Error())
			continue
		}
		if tc, ok := conn.(*net.TCPConn); ok && s.cfg.KeepAliveSec > 0 {
			_ = tc.SetKeepAlive(true)
			_ = tc.SetKeepAlivePeriod(time.Duration(s.cfg.KeepAliveSec) * time.Second)
		}
		go s.handleConn(conn)
	}
}

func (s *ProxyServer) handleConn(conn net.Conn) {
	cid := newConnID()
	defer conn.Close()

	br := bufio.NewReader(conn)
	first, err := br.Peek(1)
	if err != nil {
		s.log.Debug(fmt.Sprintf("[%s] peek: %s", cid, err.Error()))
		return
	}

	if first[0] == socksVer5 {
		s.handleSocks5(cid, conn, br)
		return
	}
	s.handleHTTP(cid, conn, br)
}

// ── SOCKS5 ──────────────────────────────────────────────────────────

func (s *ProxyServer) handleSocks5(cid string, conn net.Conn, br *bufio.Reader) {
	if err := s.socks5Handshake(br, conn); err != nil {
		s.log.Debug(fmt.Sprintf("[%s] socks5 handshake: %s", cid, err.Error()))
		return
	}

	cmd, targetAddr, targetPort, err := s.socks5ReadRequest(br, conn)
	if err != nil {
		s.log.Debug(fmt.Sprintf("[%s] socks5 request: %s", cid, err.Error()))
		return
	}

	if cmd == socksCmdUDPAssoc {
		s.handleSocks5UDPAssociate(cid, conn, br)
		return
	}

	s.log.Debug(fmt.Sprintf("[%s] SOCKS5 CONNECT %s:%d", cid, targetAddr, targetPort))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	upstream, err := DialVless(ctx, s.cfg, s.log, s.uuid, targetAddr, targetPort)
	if err != nil {
		s.log.Warn(fmt.Sprintf("[%s] connect upstream %s:%d failed: %s", cid, targetAddr, targetPort, err.Error()))
		s.socks5Reply(conn, socksRepFail)
		return
	}
	defer upstream.Close()

	if err := s.socks5Reply(conn, socksRepOK); err != nil {
		return
	}

	relay(br, conn, upstream)
	s.log.Debug(fmt.Sprintf("[%s] closed", cid))
}

// socks5Handshake 处理 SOCKS5 版本协商，仅支持 "无需认证" 方式（0x00）。
func (s *ProxyServer) socks5Handshake(br *bufio.Reader, conn net.Conn) error {
	head := make([]byte, 2)
	if _, err := io.ReadFull(br, head); err != nil {
		return err
	}
	if head[0] != socksVer5 {
		return fmt.Errorf("unsupported socks version 0x%02x", head[0])
	}
	nMethods := int(head[1])
	methods := make([]byte, nMethods)
	if _, err := io.ReadFull(br, methods); err != nil {
		return err
	}
	// 无论客户端提供哪些认证方式，一律回复 "无需认证"
	if _, err := conn.Write([]byte{socksVer5, 0x00}); err != nil {
		return err
	}
	return nil
}

// socks5ReadRequest 解析 SOCKS5 请求，返回命令类型、目标地址与端口。
func (s *ProxyServer) socks5ReadRequest(br *bufio.Reader, conn net.Conn) (byte, string, uint16, error) {
	head := make([]byte, 4)
	if _, err := io.ReadFull(br, head); err != nil {
		return 0, "", 0, err
	}
	if head[0] != socksVer5 {
		return 0, "", 0, fmt.Errorf("bad version 0x%02x", head[0])
	}
	cmd := head[1]
	if cmd != socksCmdConn && cmd != socksCmdUDPAssoc {
		s.socks5Reply(conn, socksRepCmdErr)
		return 0, "", 0, fmt.Errorf("unsupported command 0x%02x (只支持 CONNECT 和 UDP ASSOCIATE)", cmd)
	}

	var addr string
	switch head[3] {
	case socksAtypIPv4:
		b := make([]byte, 4)
		if _, err := io.ReadFull(br, b); err != nil {
			return 0, "", 0, err
		}
		addr = net.IP(b).String()
	case socksAtypDom:
		lb := make([]byte, 1)
		if _, err := io.ReadFull(br, lb); err != nil {
			return 0, "", 0, err
		}
		b := make([]byte, int(lb[0]))
		if _, err := io.ReadFull(br, b); err != nil {
			return 0, "", 0, err
		}
		addr = string(b)
	case socksAtypIPv6:
		b := make([]byte, 16)
		if _, err := io.ReadFull(br, b); err != nil {
			return 0, "", 0, err
		}
		addr = net.IP(b).String()
	default:
		s.socks5Reply(conn, socksRepCmdErr)
		return 0, "", 0, fmt.Errorf("unsupported atyp 0x%02x", head[3])
	}

	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(br, portBuf); err != nil {
		return 0, "", 0, err
	}
	port := uint16(portBuf[0])<<8 | uint16(portBuf[1])

	return cmd, addr, port, nil
}

func (s *ProxyServer) socks5Reply(conn net.Conn, rep byte) error {
	// BND.ADDR / BND.PORT 字段对 CONNECT 场景无实际意义，用 0.0.0.0:0 占位，
	// 绝大多数客户端（浏览器、curl、系统代理）都不校验这两个字段。
	reply := []byte{socksVer5, rep, 0x00, socksAtypIPv4, 0, 0, 0, 0, 0, 0}
	_, err := conn.Write(reply)
	return err
}

// socks5ReplyWithAddr 跟 socks5Reply 类似，但可以指定 BND.ADDR/BND.PORT——
// UDP ASSOCIATE 必须靠这两个字段告诉客户端"UDP 包实际发去哪个地址/端口"。
func (s *ProxyServer) socks5ReplyWithAddr(conn net.Conn, rep byte, addr *net.UDPAddr) error {
	buf := &bytes.Buffer{}
	buf.WriteByte(socksVer5)
	buf.WriteByte(rep)
	buf.WriteByte(0x00)
	ip4 := addr.IP.To4()
	if ip4 != nil {
		buf.WriteByte(socksAtypIPv4)
		buf.Write(ip4)
	} else {
		buf.WriteByte(socksAtypIPv6)
		buf.Write(addr.IP.To16())
	}
	portBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(portBuf, uint16(addr.Port))
	buf.Write(portBuf)
	_, err := conn.Write(buf.Bytes())
	return err
}

// relay 在客户端连接与上游隧道之间做双向转发，任一方向结束后
// 关闭上游连接以驱动另一方向尽快退出，并等待两个方向都结束，避免 goroutine 泄漏。
func relay(clientReader io.Reader, clientWriter io.Writer, upstream io.ReadWriteCloser) {
	errCh := make(chan error, 2)
	go func() {
		_, err := io.Copy(upstream, clientReader)
		errCh <- err
	}()
	_, _ = io.Copy(clientWriter, upstream)
	_ = upstream.Close()
	<-errCh
}
