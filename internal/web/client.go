package web

import (
	"context"
	"fmt"
	"go-code-agent/internal/security"
	"io"
	"net"
	"net/http"
	"time"
)

func safeDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("web: invalid address %q: %w", addr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		ips, lookupErr := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if lookupErr != nil || len(ips) == 0 {
			return nil, fmt.Errorf("web: could not resolve %q: %w", host, lookupErr)
		}
		var chosen net.IP
		for _, candidate := range ips {
			if err := security.CheckDialIP(candidate); err != nil {
				continue
			}
			chosen = candidate
			break
		}
		if chosen == nil {
			return nil, fmt.Errorf("web: %s resolves only to blocked addresses", host)
		}
		ip = chosen
	} else if err := security.CheckDialIP(ip); err != nil {
		return nil, fmt.Errorf("web: %w", err)
	}

	d := &net.Dialer{Timeout: 10 * time.Second}
	return d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
}

func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return fmt.Errorf("web: too many redirects (%d)", len(via))
	}
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return fmt.Errorf("web: redirect to disallowed scheme %q", req.URL.Scheme)
	}
	return nil
}

func NewSafeHTTPClient(timeout time.Duration) *http.Client {
	transport := &http.Transport{
		DialContext:           safeDialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		MaxIdleConns:          10,
	}
	return &http.Client{
		Transport:     transport,
		Timeout:       timeout,
		CheckRedirect: checkRedirect,
	}
}

func ReadLimited(r io.Reader, maxBytes int) (data []byte, truncated bool, err error) {
	limited := io.LimitReader(r, int64(maxBytes)+1)
	data, err = io.ReadAll(limited)
	if err != nil {
		return data, false, err
	}
	if len(data) > maxBytes {
		return data[:maxBytes], true, nil
	}
	return data, false, nil
}
