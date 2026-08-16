package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"time"
)

const udpTunnelIdleTimeout = 60 * time.Second

func parseSocks5UDPPacket(pkt []byte) (addr string, port uint16, payload []byte, err error) {
	if len(pkt) < 4 {
		return "", 0, nil, fmt.Errorf("udp packet too short")
	}
	if pkt[2] != 0x00 {
		return "", 0, nil, fmt.Errorf("fragmented udp packet not supported")
	}
	atyp := pkt[3]
	idx := 4
	switch atyp {
	case socksAtypIPv4:
		if len(pkt) < idx+4+2 {
			return "", 0, nil, fmt.Errorf("truncated ipv4 udp packet")
		}
		addr = net.IP(pkt[idx : idx+4]).String()
		idx += 4
	case socksAtypDom:
		if len(pkt) < idx+1 {
			return "", 0, nil, fmt.Errorf("truncated domain length")
		}
		l := int(pkt[idx])
		idx++
		if len(pkt) < idx+l+2 {
			return "", 0, nil, fmt.Errorf("truncated domain udp packet")
		}
		addr = string(pkt[idx : idx+l])
		idx += l
	case socksAtypIPv6:
		if len(pkt) < idx+16+2 {
			return "", 0, nil, fmt.Errorf("truncated ipv6 udp packet")
		}
		addr = net.IP(pkt[idx : idx+16]).String()
		idx += 16
	default:
		return "", 0, nil, fmt.Errorf("unsupported atyp 0x%02x", atyp)
	}
	port = binary.BigEndian.Uint16(pkt[idx : idx+2])
	idx += 2
	payload = pkt[idx:]
	return addr, port, payload, nil
}

func buildSocks5UDPPacket(addr string, port uint16, payload []byte) []byte {
	buf := &bytes.Buffer{}
	buf.Write([]byte{0x00, 0x00, 0x00})
	if ip := net.ParseIP(addr); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			buf.WriteByte(socksAtypIPv4)
			buf.Write(ip4)
		} else {
			buf.WriteByte(socksAtypIPv6)
			buf.Write(ip.To16())
		}
	} else {
		buf.WriteByte(socksAtypDom)
		buf.WriteByte(byte(len(addr)))
		buf.WriteString(addr)
	}
	portBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(portBuf, port)
	buf.Write(portBuf)
	buf.Write(payload)
	return buf.Bytes()
}

type udpTunnel struct {
	conn net.Conn
}

func (s *ProxyServer) handleSocks5UDPAssociate(cid string, ctrlConn net.Conn, br *bufio.Reader) {
	bindIP := net.ParseIP(s.cfg.LocalIP)
	if bindIP == nil {
		bindIP = net.IPv4(127, 0, 0, 1)
	}
	localUDPConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: bindIP, Port: 0})
	if err != nil {
		s.log.Warn(fmt.Sprintf("[%s] udp associate listen failed: %s", cid, err.Error()))
		s.socks5Reply(ctrlConn, socksRepFail)
		return
	}
	defer localUDPConn.Close()

	localAddr := localUDPConn.LocalAddr().(*net.UDPAddr)
	replyAddr := &net.UDPAddr{IP: localAddr.IP, Port: localAddr.Port}
	if replyAddr.IP.IsUnspecified() {
		replyAddr.IP = net.ParseIP("127.0.0.1")
	}
	if err := s.socks5ReplyWithAddr(ctrlConn, socksRepOK, replyAddr); err != nil {
		return
	}
	s.log.Debug(fmt.Sprintf("[%s] UDP ASSOCIATE ready on %s", cid, localAddr.String()))

	var mu sync.Mutex
	tunnels := make(map[string]*udpTunnel)
	var clientAddr *net.UDPAddr

	defer func() {
		mu.Lock()
		for _, t := range tunnels {
			t.conn.Close()
		}
		mu.Unlock()
	}()

	ctrlClosed := make(chan struct{})
	go func() {
		discard := make([]byte, 1)
		for {
			if _, err := br.Read(discard); err != nil {
				close(ctrlClosed)
				return
			}
		}
	}()

	getOrCreateTunnel := func(destAddr string, destPort uint16) (*udpTunnel, error) {
		key := net.JoinHostPort(destAddr, fmt.Sprintf("%d", destPort))

		mu.Lock()
		if t, ok := tunnels[key]; ok {
			mu.Unlock()
			return t, nil
		}
		mu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(s.cfg.DialTimeoutMS)*time.Millisecond)
		conn, err := DialVlessUDP(ctx, s.cfg, s.log, s.uuid, destAddr, destPort)
		cancel()
		if err != nil {
			return nil, err
		}

		t := &udpTunnel{conn: conn}
		mu.Lock()
		tunnels[key] = t
		mu.Unlock()

		go func() {
			defer func() {
				mu.Lock()
				delete(tunnels, key)
				mu.Unlock()
				conn.Close()
			}()
			for {
				_ = conn.SetReadDeadline(time.Now().Add(udpTunnelIdleTimeout))
				payload, err := readUDPFrameClient(conn)
				if err != nil {
					return
				}
				mu.Lock()
				peer := clientAddr
				mu.Unlock()
				if peer == nil {
					continue
				}
				pkt := buildSocks5UDPPacket(destAddr, destPort, payload)
				_, _ = localUDPConn.WriteToUDP(pkt, peer)
			}
		}()

		return t, nil
	}

	readBuf := make([]byte, 65535)
	for {
		select {
		case <-ctrlClosed:
			return
		default:
		}

		_ = localUDPConn.SetReadDeadline(time.Now().Add(time.Second))
		n, from, err := localUDPConn.ReadFromUDP(readBuf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}

		mu.Lock()
		if clientAddr == nil {
			clientAddr = from
		}
		mu.Unlock()

		destAddr, destPort, payload, perr := parseSocks5UDPPacket(readBuf[:n])
		if perr != nil {
			s.log.Debug(fmt.Sprintf("[%s] bad udp packet: %s", cid, perr.Error()))
			continue
		}

		tunnel, terr := getOrCreateTunnel(destAddr, destPort)
		if terr != nil {
			s.log.Debug(fmt.Sprintf("[%s] udp tunnel to %s:%d failed: %s", cid, destAddr, destPort, terr.Error()))
			continue
		}

		if werr := writeUDPFrameClient(tunnel.conn, payload); werr != nil {
			s.log.Debug(fmt.Sprintf("[%s] udp tunnel write failed: %s", cid, werr.Error()))
		}
	}
}
