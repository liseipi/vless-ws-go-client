package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
)

// handleHTTP 处理与 SOCKS5 共用同一端口的 HTTP(S) 代理请求：
//   - CONNECT method  -> 用于 HTTPS：回 200 后转为透明双向隧道
//   - 其他 method（绝对形式 URI，如 "GET http://host/path"）-> 普通 HTTP 代理转发
func (s *ProxyServer) handleHTTP(cid string, conn net.Conn, br *bufio.Reader) {
	req, err := http.ReadRequest(br)
	if err != nil {
		s.log.Debug(fmt.Sprintf("[%s] http read request: %s", cid, err.Error()))
		return
	}

	if req.Method == http.MethodConnect {
		s.handleHTTPConnect(cid, conn, br, req)
		return
	}
	s.handlePlainHTTP(cid, conn, req)
}

// handleHTTPConnect 处理 HTTP CONNECT（浏览器访问 HTTPS 站点时使用），
// 回复 200 之后整条连接就是一条透明隧道，与 SOCKS5 CONNECT 语义相同。
func (s *ProxyServer) handleHTTPConnect(cid string, conn net.Conn, br *bufio.Reader, req *http.Request) {
	targetAddr, targetPort, err := splitHostPort(req.Host, 443)
	if err != nil {
		s.log.Debug(fmt.Sprintf("[%s] connect target: %s", cid, err.Error()))
		fmt.Fprintf(conn, "HTTP/1.1 400 Bad Request\r\n\r\n")
		return
	}

	s.log.Debug(fmt.Sprintf("[%s] HTTP CONNECT %s:%d", cid, targetAddr, targetPort))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	upstream, err := DialVless(ctx, s.sessMgr, s.log, s.uuid, targetAddr, targetPort)
	if err != nil {
		s.log.Warn(fmt.Sprintf("[%s] connect upstream %s:%d failed: %s", cid, targetAddr, targetPort, err.Error()))
		fmt.Fprintf(conn, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer upstream.Close()

	if _, err := fmt.Fprintf(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}

	relay(br, conn, upstream)
	s.log.Debug(fmt.Sprintf("[%s] closed", cid))
}

// handlePlainHTTP 处理普通（非 CONNECT）HTTP 代理请求，例如
// "GET http://example.com/ HTTP/1.1"。为简化实现，每条连接只转发一个请求
// （强制 Connection: close），足以覆盖 curl -x / 大多数工具的基本用法。
func (s *ProxyServer) handlePlainHTTP(cid string, conn net.Conn, req *http.Request) {
	if req.URL == nil || req.URL.Host == "" {
		fmt.Fprintf(conn, "HTTP/1.1 400 Bad Request\r\n\r\nmissing absolute-form URI (is this client configured as an HTTP proxy?)")
		return
	}

	targetAddr, targetPort, err := splitHostPort(req.URL.Host, 80)
	if err != nil {
		fmt.Fprintf(conn, "HTTP/1.1 400 Bad Request\r\n\r\n")
		return
	}

	s.log.Debug(fmt.Sprintf("[%s] HTTP %s %s:%d%s", cid, req.Method, targetAddr, targetPort, req.URL.Path))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	upstream, err := DialVless(ctx, s.sessMgr, s.log, s.uuid, targetAddr, targetPort)
	if err != nil {
		s.log.Warn(fmt.Sprintf("[%s] connect upstream %s:%d failed: %s", cid, targetAddr, targetPort, err.Error()))
		fmt.Fprintf(conn, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer upstream.Close()

	req.Close = true
	req.Header.Set("Connection", "close")
	// RequestURI 是服务端解析请求时的字段，客户端形式的 Request.Write 不允许设置它，清空以复用标准库序列化。
	req.RequestURI = ""

	if err := req.Write(upstream); err != nil {
		s.log.Debug(fmt.Sprintf("[%s] write upstream request: %s", cid, err.Error()))
		return
	}

	if _, err := io.Copy(conn, upstream); err != nil {
		s.log.Debug(fmt.Sprintf("[%s] copy upstream response: %s", cid, err.Error()))
	}
	s.log.Debug(fmt.Sprintf("[%s] closed", cid))
}

// splitHostPort 拆分 host（可能不带端口）为地址和端口，不带端口时使用 defPort。
func splitHostPort(hostport string, defPort int) (string, uint16, error) {
	host, portStr, err := net.SplitHostPort(hostport)
	if err != nil {
		// 没有端口部分
		host = hostport
		portStr = strconv.Itoa(defPort)
	}
	if host == "" {
		return "", 0, fmt.Errorf("empty host")
	}
	p, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return "", 0, fmt.Errorf("invalid port %q", portStr)
	}
	return host, uint16(p), nil
}
