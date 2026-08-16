package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
)

const (
	vlessVer   byte = 0x00
	cmdTCP     byte = 0x01
	cmdUDP     byte = 0x02
	atypIPv4   byte = 0x01
	atypDomain byte = 0x02
	atypIPv6   byte = 0x03
)

// uuidToBytes 把标准 UUID 字符串（带或不带连字符）转换为 16 字节数组，
// 与服务端 vless.go 中的实现保持一致。
func uuidToBytes(uuid string) ([16]byte, error) {
	var out [16]byte
	hex := make([]byte, 0, 32)
	for i := 0; i < len(uuid); i++ {
		c := uuid[i]
		if c == '-' {
			continue
		}
		hex = append(hex, c)
	}
	if len(hex) != 32 {
		return out, fmt.Errorf("invalid uuid: %s", uuid)
	}
	n := len(hex) / 2
	for i := 0; i < n; i++ {
		hi, err := hexVal(hex[i*2])
		if err != nil {
			return out, err
		}
		lo, err := hexVal(hex[i*2+1])
		if err != nil {
			return out, err
		}
		out[i] = hi<<4 | lo
	}
	return out, nil
}

func hexVal(c byte) (byte, error) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', nil
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, nil
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, nil
	default:
		return 0, fmt.Errorf("invalid hex char: %c", c)
	}
}

// buildVlessHeader 按服务端 parseVlessHeader 期望的格式构造请求头：
// ver(1) + uuid(16) + addonsLen(1)=0 + cmd(1) + port(2,BE) + atyp(1) + addr
//
// addr 可以是域名、IPv4 或 IPv6 字符串，会自动选择合适的 atyp。
func buildVlessHeader(uuid [16]byte, cmd byte, targetAddr string, targetPort uint16) ([]byte, error) {
	buf := &bytes.Buffer{}
	buf.WriteByte(vlessVer)
	buf.Write(uuid[:])
	buf.WriteByte(0x00) // addons length = 0，服务端不解析额外字段
	buf.WriteByte(cmd)

	portBytes := make([]byte, 2)
	binary.BigEndian.PutUint16(portBytes, targetPort)
	buf.Write(portBytes)

	if ip := net.ParseIP(targetAddr); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			buf.WriteByte(atypIPv4)
			buf.Write(ip4)
		} else {
			ip16 := ip.To16()
			if ip16 == nil {
				return nil, fmt.Errorf("invalid ip address: %s", targetAddr)
			}
			buf.WriteByte(atypIPv6)
			buf.Write(ip16)
		}
	} else {
		if len(targetAddr) == 0 || len(targetAddr) > 255 {
			return nil, fmt.Errorf("invalid domain length: %s", targetAddr)
		}
		buf.WriteByte(atypDomain)
		buf.WriteByte(byte(len(targetAddr)))
		buf.WriteString(targetAddr)
	}

	return buf.Bytes(), nil
}

// readVlessResponseHeader 从已建立的连接中读取服务端返回的
// VLESS 响应头：ver(1) + addonsLen(1) + addons(addonsLen)。
// 返回值为需要额外读取的 addons 字节数（通常为 0）。
func parseVlessRespHeader(b []byte) (addonsLen int, headerLen int, err error) {
	if len(b) < 2 {
		return 0, 0, fmt.Errorf("response too short")
	}
	if b[0] != vlessVer {
		return 0, 0, fmt.Errorf("bad response version 0x%02x", b[0])
	}
	addonsLen = int(b[1])
	headerLen = 2 + addonsLen
	if len(b) < headerLen {
		return addonsLen, headerLen, fmt.Errorf("need more data")
	}
	return addonsLen, headerLen, nil
}
