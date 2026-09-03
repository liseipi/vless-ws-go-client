package main

import (
	"context"
	"fmt"
	"net"
	"time"

	wgtun "golang.zx2c4.com/wireguard/tun"

	"github.com/sagernet/gvisor/pkg/buffer"
	"github.com/sagernet/gvisor/pkg/tcpip"
	"github.com/sagernet/gvisor/pkg/tcpip/adapters/gonet"
	"github.com/sagernet/gvisor/pkg/tcpip/header"
	"github.com/sagernet/gvisor/pkg/tcpip/link/channel"
	"github.com/sagernet/gvisor/pkg/tcpip/network/ipv4"
	"github.com/sagernet/gvisor/pkg/tcpip/network/ipv6"
	"github.com/sagernet/gvisor/pkg/tcpip/stack"
	"github.com/sagernet/gvisor/pkg/tcpip/transport/tcp"
	"github.com/sagernet/gvisor/pkg/tcpip/transport/udp"
	"github.com/sagernet/gvisor/pkg/waiter"
)

// TunServer 创建一张 tun 虚拟网卡，把系统交给这张网卡的全部 IP 层流量接入
// 一个用户态 TCP/IP 协议栈（gVisor netstack），协议栈把流量拆解成一条条
// 独立的 TCP 连接 / UDP 会话之后，直接复用 vlessconn.go 里现成的
// DialVless / DialVlessUDP 转发进 VLESS 隧道——效果上等价于系统级 "全局代理"：
// 任何写往这张网卡的流量都会被当成普通请求重新转发一遍，不再需要应用自己
// 支持 SOCKS5。
//
// 用法上和 ProxyServer 是平级的两种前端，二者共用同一个 sessionManager 也
//完全可以同时启用（比如一边跑 SOCKS5 给部分程序用，一边跑 tun 接管系统级
// 流量），互不冲突。
type TunServer struct {
	cfg     *Config
	log     *Logger
	uuid    [16]byte
	sessMgr *sessionManager
}

func NewTunServer(cfg *Config, log *Logger, uuid [16]byte) *TunServer {
	return &TunServer{cfg: cfg, log: log, uuid: uuid, sessMgr: newSessionManager(cfg, log)}
}

const tunNICID = tcpip.NICID(1)

// ListenAndServe 创建 tun 网卡并启动协议栈，阻塞运行直到出错。
func (s *TunServer) ListenAndServe() error {
	dev, err := wgtun.CreateTUN(s.cfg.TunName, s.cfg.TunMTU)
	if err != nil {
		return fmt.Errorf("create tun %q: %w", s.cfg.TunName, err)
	}
	realName, _ := dev.Name()
	s.log.Info(fmt.Sprintf("tun 网卡已创建: %s (请求名 %q, mtu=%d)", realName, s.cfg.TunName, s.cfg.TunMTU))

	linkEP := channel.New(512 /*队列长度*/, uint32(s.cfg.TunMTU), "" /*不需要链路层地址*/)

	ns := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	if terr := ns.CreateNIC(tunNICID, linkEP); terr != nil {
		return fmt.Errorf("create nic: %s", terr)
	}
	// tun 网卡上看到的目标地址是系统里任意一个想访问的公网地址，而不是
	// 协议栈自己的地址，必须关掉"目标地址必须是本机地址才处理"的默认限制，
	// 并允许以并非本机拥有的源地址发出数据（转发场景的标准做法）。
	if terr := ns.SetPromiscuousMode(tunNICID, true); terr != nil {
		return fmt.Errorf("set promiscuous: %s", terr)
	}
	if terr := ns.SetSpoofing(tunNICID, true); terr != nil {
		return fmt.Errorf("set spoofing: %s", terr)
	}
	ns.SetRouteTable([]tcpip.Route{
		{Destination: header.IPv4EmptySubnet, NIC: tunNICID},
		{Destination: header.IPv6EmptySubnet, NIC: tunNICID},
	})

	// TCP：每一个新连接都会触发一次回调，在回调里取出真实的目的地址/端口，
	// 拨向 VLESS 隧道，然后双向转发。
	tcpFwd := tcp.NewForwarder(ns, 0 /*rcvWnd，0 表示使用默认值*/, 2048 /*最大同时处理的半连接数*/, s.handleTCP)
	ns.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpFwd.HandlePacket)

	// UDP：gVisor 没有连接的概念，每个 (源, 目的) 二元组第一次出现时触发一次
	// 回调，我们在回调里创建一条 VLESS UDP 隧道并持续双向转发。
	udpFwd := udp.NewForwarder(ns, s.handleUDP)
	ns.SetTransportProtocolHandler(udp.ProtocolNumber, udpFwd.HandlePacket)

	if s.cfg.TunAddr4 != "" {
		if ip4 := net.ParseIP(s.cfg.TunAddr4).To4(); ip4 != nil {
			protoAddr := tcpip.ProtocolAddress{
				Protocol: ipv4.ProtocolNumber,
				AddressWithPrefix: tcpip.AddressWithPrefix{
					Address:   tcpip.AddrFromSlice(ip4),
					PrefixLen: 32,
				},
			}
			if terr := ns.AddProtocolAddress(tunNICID, protoAddr, stack.AddressProperties{}); terr != nil {
				return fmt.Errorf("add ipv4 address %s: %s", s.cfg.TunAddr4, terr)
			}
		}
	}
	if s.cfg.TunAddr6 != "" {
		if ip6 := net.ParseIP(s.cfg.TunAddr6).To16(); ip6 != nil {
			protoAddr := tcpip.ProtocolAddress{
				Protocol: ipv6.ProtocolNumber,
				AddressWithPrefix: tcpip.AddressWithPrefix{
					Address:   tcpip.AddrFromSlice(ip6),
					PrefixLen: 128,
				},
			}
			if terr := ns.AddProtocolAddress(tunNICID, protoAddr, stack.AddressProperties{}); terr != nil {
				return fmt.Errorf("add ipv6 address %s: %s", s.cfg.TunAddr6, terr)
			}
		}
	}

	stop := make(chan struct{})
	go s.pumpDeviceToStack(dev, linkEP, stop)
	go s.pumpStackToDevice(dev, linkEP, stop)

	s.log.Info(fmt.Sprintf("TUN 模式已启动：网卡 %s，网卡地址 %s，请按下方说明配置系统路由/DNS", realName, s.cfg.TunAddr4))
	<-stop
	return fmt.Errorf("tun device closed")
}

// pumpDeviceToStack 不断从 tun 设备读取系统写入的 IP 包，注入协议栈处理。
func (s *TunServer) pumpDeviceToStack(dev wgtun.Device, ep *channel.Endpoint, stop chan struct{}) {
	defer close(stop)
	// 预留一点前导空间，wireguard-go 的部分平台实现要求 offset>0。
	const offset = 16
	buf := make([]byte, offset+s.cfg.TunMTU+64)
	for {
		n, err := dev.Read(buf, offset)
		if err != nil {
			s.log.Warn(fmt.Sprintf("tun read: %s", err.Error()))
			return
		}
		if n == 0 {
			continue
		}
		pkt := buf[offset : offset+n]
		if len(pkt) < 1 {
			continue
		}
		var proto tcpip.NetworkProtocolNumber
		switch pkt[0] >> 4 {
		case 4:
			proto = ipv4.ProtocolNumber
		case 6:
			proto = ipv6.ProtocolNumber
		default:
			continue // 既不是 IPv4 也不是 IPv6，丢弃
		}

		payload := make([]byte, len(pkt))
		copy(payload, pkt)
		pb := stack.NewPacketBuffer(stack.PacketBufferOptions{
			Payload: buffer.MakeWithData(payload),
		})
		ep.InjectInbound(proto, pb)
		pb.DecRef()
	}
}

// pumpStackToDevice 不断把协议栈产生的出站 IP 包（比如 TCP ACK、握手响应，
// 以及我们从 VLESS 隧道收到、转发回本机应用的数据）写回 tun 设备。
func (s *TunServer) pumpStackToDevice(dev wgtun.Device, ep *channel.Endpoint, stop chan struct{}) {
	const offset = 16
	writeBuf := make([]byte, offset+s.cfg.TunMTU+64)
	for {
		pb := ep.ReadContext(context.Background())
		if pb.IsNil() {
			select {
			case <-stop:
				return
			default:
				continue
			}
		}
		buf := pb.ToBuffer()
		data := buf.Flatten()
		pb.DecRef()

		if len(data) > cap(writeBuf)-offset {
			continue // 超过 MTU 的异常包，直接丢弃
		}
		n := copy(writeBuf[offset:], data)
		if _, err := dev.Write(writeBuf[:offset+n], offset); err != nil {
			s.log.Warn(fmt.Sprintf("tun write: %s", err.Error()))
			return
		}
	}
}

// handleTCP 是 gVisor tcp.Forwarder 的回调：协议栈每观察到一个新的、此前没
// 见过的 TCP 四元组（且携带 SYN）就调用一次。ForwarderRequest.ID() 里的
// "Local" 是相对于协议栈自身而言的——也就是这个 TCP 连接原本要去的目的地址
// （请求方眼里的"远端"）；"Remote" 才是发起连接的那个本机应用的地址。
func (s *TunServer) handleTCP(r *tcp.ForwarderRequest) {
	id := r.ID()
	targetAddr := id.LocalAddress.String()
	targetPort := id.LocalPort

	var wq waiter.Queue
	ep, terr := r.CreateEndpoint(&wq)
	if terr != nil {
		s.log.Debug(fmt.Sprintf("[tun] tcp create endpoint %s:%d: %s", targetAddr, targetPort, terr))
		r.Complete(true)
		return
	}
	r.Complete(false)

	local := gonet.NewTCPConn(&wq, ep)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	upstream, err := DialVless(ctx, s.sessMgr, s.log, s.uuid, targetAddr, targetPort)
	if err != nil {
		s.log.Warn(fmt.Sprintf("[tun] tcp connect upstream %s:%d failed: %s", targetAddr, targetPort, err.Error()))
		local.Close()
		return
	}

	s.log.Debug(fmt.Sprintf("[tun] TCP %s:%d", targetAddr, targetPort))
	relay(local, local, upstream)
}

// handleUDP 是 gVisor udp.Forwarder 的回调，语义同 handleTCP：Local 是目的
// 地址，Remote 是发起请求的本机应用地址。每个 (源地址, 目的地址) 二元组只
// 会触发一次，我们借此机会创建一条 VLESS UDP 隧道，并起一个 goroutine 持续
// 把隧道收到的数据回写给本机应用；由 gonet.UDPConn 承担与本机应用之间的
// 收发。
func (s *TunServer) handleUDP(r *udp.ForwarderRequest) {
	id := r.ID()
	targetAddr := id.LocalAddress.String()
	targetPort := id.LocalPort

	var wq waiter.Queue
	ep, terr := r.CreateEndpoint(&wq)
	if terr != nil {
		s.log.Debug(fmt.Sprintf("[tun] udp create endpoint %s:%d: %s", targetAddr, targetPort, terr))
		return
	}
	local := gonet.NewUDPConn(nil, &wq, ep)

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(s.cfg.DialTimeoutMS)*time.Millisecond)
	upstream, err := DialVlessUDP(ctx, s.sessMgr, s.log, s.uuid, targetAddr, targetPort)
	cancel()
	if err != nil {
		s.log.Debug(fmt.Sprintf("[tun] udp connect upstream %s:%d failed: %s", targetAddr, targetPort, err.Error()))
		local.Close()
		return
	}

	s.log.Debug(fmt.Sprintf("[tun] UDP %s:%d", targetAddr, targetPort))

	done := make(chan struct{})

	// 本机应用 -> VLESS 隧道
	go func() {
		defer close(done)
		buf := make([]byte, 65535)
		for {
			_ = local.SetReadDeadline(time.Now().Add(udpTunnelIdleTimeout))
			n, err := local.Read(buf)
			if err != nil {
				return
			}
			if werr := writeUDPFrameClient(upstream, buf[:n]); werr != nil {
				return
			}
		}
	}()

	// VLESS 隧道 -> 本机应用
	go func() {
		for {
			_ = upstream.SetReadDeadline(time.Now().Add(udpTunnelIdleTimeout))
			payload, err := readUDPFrameClient(upstream)
			if err != nil {
				local.Close()
				return
			}
			if _, err := local.Write(payload); err != nil {
				return
			}
		}
	}()

	<-done
	upstream.Close()
	local.Close()
}
