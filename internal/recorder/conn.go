package recorder

import (
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"sync"
	"time"
)

// capture records the raw bytes of one HTTP exchange on one connection.
type capture struct {
	mu       sync.Mutex
	sent     bytes.Buffer
	received bytes.Buffer
	limit    int64
	over     bool
	remoteIP string
}

func (c *capture) write(p []byte) {
	c.mu.Lock()
	c.sent.Write(p)
	c.mu.Unlock()
}

// unwrite drops the last n recorded bytes that were not actually sent.
func (c *capture) unwrite(n int) {
	c.mu.Lock()
	c.sent.Truncate(max(0, c.sent.Len()-n))
	c.mu.Unlock()
}

func (c *capture) read(p []byte) {
	c.mu.Lock()
	if c.limit > 0 && int64(c.received.Len()+len(p)) > c.limit {
		c.over = true
		p = p[:max(0, int(c.limit)-c.received.Len())]
	}
	c.received.Write(p)
	c.mu.Unlock()
}

func (c *capture) snapshot() (sent, received []byte, truncated bool, ip string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return bytes.Clone(c.sent.Bytes()), bytes.Clone(c.received.Bytes()), c.over, c.remoteIP
}

type captureConn struct {
	net.Conn
	c *capture
}

func (cc *captureConn) Read(p []byte) (int, error) {
	n, err := cc.Conn.Read(p)
	if n > 0 {
		cc.c.read(p[:n])
	}
	return n, err
}

// Write records p before sending it: the server may answer, and the response
// be read and snapshotted, before the transport's writer goroutine returns.
func (cc *captureConn) Write(p []byte) (int, error) {
	cc.c.write(p)
	n, err := cc.Conn.Write(p)
	if n < len(p) {
		cc.c.unwrite(len(p) - n)
	}
	return n, err
}

// transportFor returns a one-shot HTTP/1.1 transport whose single connection
// is recorded into c. Compression is left to the caller so raw bytes are kept.
func transportFor(c *capture, opts *Options) *http.Transport {
	dialer := &net.Dialer{Timeout: opts.DialTimeout, KeepAlive: -1}
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		if opts.Dial != nil {
			return opts.Dial(ctx, network, addr)
		}
		return dialer.DialContext(ctx, network, addr)
	}
	setIP := func(conn net.Conn) {
		if host, _, err := net.SplitHostPort(conn.RemoteAddr().String()); err == nil {
			c.mu.Lock()
			c.remoteIP = host
			c.mu.Unlock()
		}
	}
	return &http.Transport{
		DisableKeepAlives:     true,
		DisableCompression:    true,
		ForceAttemptHTTP2:     false,
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{},
		ResponseHeaderTimeout: opts.ResponseTimeout,
		Proxy:                 nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dial(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			setIP(conn)
			return &captureConn{Conn: conn, c: c}, nil
		},
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			raw, err := dial(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			setIP(raw)
			host, _, _ := net.SplitHostPort(addr)
			cfg := &tls.Config{ServerName: host, NextProtos: []string{"http/1.1"}, InsecureSkipVerify: opts.InsecureTLS}
			if opts.TLSConfig != nil {
				cfg = opts.TLSConfig.Clone()
				if cfg.ServerName == "" {
					cfg.ServerName = host
				}
				cfg.NextProtos = []string{"http/1.1"}
			}
			tc := tls.Client(raw, cfg)
			hctx, cancel := context.WithTimeout(ctx, max(opts.DialTimeout, time.Second))
			defer cancel()
			if err := tc.HandshakeContext(hctx); err != nil {
				raw.Close()
				return nil, err
			}
			return &captureConn{Conn: tc, c: c}, nil
		},
	}
}
