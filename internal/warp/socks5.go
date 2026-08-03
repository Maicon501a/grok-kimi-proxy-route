package warp

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

const socks5Timeout = 15 * time.Second

// dialSOCKS5 opens a no-auth SOCKS5 CONNECT tunnel. WARP's proxy mode only
// exposes a local SOCKS5 endpoint; TLS is layered by http.Transport afterwards.
func dialSOCKS5(ctx context.Context, proxyAddr, targetAddr string) (conn net.Conn, err error) {
	dialer := net.Dialer{Timeout: socks5Timeout}
	conn, err = dialer.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, err
	}
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()
	defer func() {
		if err != nil {
			_ = conn.Close()
		}
	}()
	_ = conn.SetDeadline(time.Now().Add(socks5Timeout))

	if _, err = conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		return nil, err
	}
	var greeting [2]byte
	if _, err = io.ReadFull(conn, greeting[:]); err != nil {
		return nil, err
	}
	if greeting[0] != 0x05 || greeting[1] != 0x00 {
		return nil, fmt.Errorf("WARP SOCKS5 auth rejected: version=%d method=%d", greeting[0], greeting[1])
	}

	host, portText, err := net.SplitHostPort(targetAddr)
	if err != nil {
		return nil, fmt.Errorf("invalid SOCKS5 target %q: %w", targetAddr, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("invalid SOCKS5 target port %q", portText)
	}

	request := []byte{0x05, 0x01, 0x00}
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			request = append(request, 0x01)
			request = append(request, ip4...)
		} else {
			request = append(request, 0x04)
			request = append(request, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return nil, fmt.Errorf("SOCKS5 target hostname too long")
		}
		request = append(request, 0x03, byte(len(host)))
		request = append(request, []byte(host)...)
	}
	var portBytes [2]byte
	binary.BigEndian.PutUint16(portBytes[:], uint16(port))
	request = append(request, portBytes[:]...)
	if _, err = conn.Write(request); err != nil {
		return nil, err
	}

	var header [4]byte
	if _, err = io.ReadFull(conn, header[:]); err != nil {
		return nil, err
	}
	if header[0] != 0x05 || header[1] != 0x00 {
		return nil, fmt.Errorf("SOCKS5 CONNECT failed: version=%d reply=%d", header[0], header[1])
	}
	var addressLen int
	switch header[3] {
	case 0x01:
		addressLen = 4
	case 0x04:
		addressLen = 16
	case 0x03:
		var n [1]byte
		if _, err = io.ReadFull(conn, n[:]); err != nil {
			return nil, err
		}
		addressLen = int(n[0])
	default:
		return nil, fmt.Errorf("SOCKS5 bad reply ATYP=%d", header[3])
	}
	if addressLen > 0 {
		if _, err = io.CopyN(io.Discard, conn, int64(addressLen)); err != nil {
			return nil, err
		}
	}
	var boundPort [2]byte
	if _, err = io.ReadFull(conn, boundPort[:]); err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

func socksProxyAddress(host string, port int) string {
	return net.JoinHostPort(strings.TrimSpace(host), strconv.Itoa(port))
}
