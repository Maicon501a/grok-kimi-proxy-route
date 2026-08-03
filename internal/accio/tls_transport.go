package accio

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

// newChromeTLSClient creates an *http.Client that spoofs a Chrome TLS
// fingerprint (JA3/JA4) using utls AND speaks HTTP/2, matching what the
// Alibaba phoenix gateway expects.
//
// The gateway (phoenix-gw.alibaba.com) negotiates ALPN "h2" regardless of
// the ALPN list the client offers, then sends an HTTP/2 server preface.
// net/http's Transport cannot use HTTP/2 over a custom DialTLSContext
// (it type-asserts *crypto/tls.Conn), which previously produced:
//
//	net/http: HTTP/1.x transport connection broken: malformed HTTP
//	response "\x00\x00\x12\x04..."  (an HTTP/2 SETTINGS frame)
//
// golang.org/x/net/http2's Transport accepts any net.Conn returned from
// DialTLSContext, so it can be paired with the utls Chrome fingerprint.
func newChromeTLSClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	dialChromeTLS := func(ctx context.Context, network, addr string) (net.Conn, error) {
		rawConn, err := dialer.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
		}
		_ = rawConn.SetDeadline(time.Now().Add(20 * time.Second))
		tlsConn := utls.UClient(rawConn, &utls.Config{
			ServerName:         host,
			NextProtos:         []string{"h2", "http/1.1"},
			InsecureSkipVerify: false,
		}, utls.HelloChrome_120)
		if err := tlsConn.Handshake(); err != nil {
			rawConn.Close()
			return nil, err
		}
		_ = rawConn.SetDeadline(time.Time{})
		return tlsConn, nil
	}

	tr := &http2.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			return dialChromeTLS(ctx, network, addr)
		},
	}

	return &http.Client{
		Transport: tr,
		Timeout:   timeout,
	}
}
