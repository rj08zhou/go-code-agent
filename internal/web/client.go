// Package web provides the agent's outbound network access - an
// SSRF-hardened HTTP client shared by the web_fetch and web_search
// tools, plus HTML-to-text extraction and pluggable search backends.
//
// Everything in this package that makes a network request MUST go
// through NewSafeHTTPClient (or a client built from it): that is the
// single choke point that enforces the project's SSRF policy (see
// internal/security/ssrf.go) by validating every IP right before the
// TCP connection is opened, on every redirect hop, not just the
// initial request's hostname.
package web

import (
	"context"
	"fmt"
	"go-code-agent/internal/security"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// safeDialContext wraps the standard dialer so every outbound
// connection - regardless of which net/http code path triggers it
// (initial request, redirect, keep-alive reconnect) - is checked
// against the SSRF policy on the ACTUAL resolved IP, not the
// hostname string. This is what defends against DNS rebinding: a
// hostname's DNS answer can differ between "looks safe" and "connects
// somewhere private", so the only trustworthy check point is here,
// where net/http hands us the concrete address it resolved.
func safeDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("web: invalid address %q: %w", addr, err)
	}

	// addr may already be a literal IP (net/http's own resolution) or
	// a hostname (some callers). Resolve explicitly so we always have
	// a concrete IP to check, then dial that IP directly - this also
	// prevents a second, unchecked DNS lookup from happening on the
	// literal-IP dial call below and potentially returning a
	// different answer than the one we just validated.
	ip := net.ParseIP(host)
	if ip == nil {
		ips, lookupErr := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if lookupErr != nil || len(ips) == 0 {
			return nil, fmt.Errorf("web: could not resolve %q: %w", host, lookupErr)
		}
		// Check ALL resolved addresses, not just the first: some
		// resolvers/attackers can return a public IP first and a
		// private one second, and net/http's own dialer may pick any
		// of them under Happy Eyeballs. Fail closed if any is blocked.
		for _, candidate := range ips {
			if err := security.CheckDialIP(candidate); err != nil {
				return nil, fmt.Errorf("web: %s resolves to blocked address %s: %w", host, candidate, err)
			}
		}
		ip = ips[0]
	} else if err := security.CheckDialIP(ip); err != nil {
		return nil, err
	}

	d := &net.Dialer{Timeout: 10 * time.Second}
	return d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
}

// checkRedirect is installed as the http.Client's CheckRedirect hook.
// The DialContext above already validates the IP of every connection
// net/http actually opens (including redirect targets), so this hook
// is a defense-in-depth belt-and-suspenders layer that rejects
// non-http(s) redirect schemes up front (before even attempting to
// resolve/dial them) and caps redirect chain length.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return fmt.Errorf("web: too many redirects (%d)", len(via))
	}
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return fmt.Errorf("web: redirect to disallowed scheme %q", req.URL.Scheme)
	}
	return nil
}

// NewSafeHTTPClient returns an *http.Client whose Transport dials
// through safeDialContext (SSRF policy enforced on every connection,
// including redirect hops) and whose CheckRedirect rejects non-http(s)
// redirects and caps chain length. timeout bounds the whole
// request/response cycle (including any redirects followed).
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

// ValidateRequestURL performs the scheme/emptiness checks that should
// reject an obviously-bad URL before any DNS lookup or connection is
// attempted at all - a fast, allocation-free rejection path for the
// common "model passed garbage" case, distinct from the DNS-rebinding
// -proof runtime check in safeDialContext (which is the check that
// actually matters for security; this one is just an early exit).
func ValidateRequestURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("url is empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported scheme %q (only http/https allowed)", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("url has no host")
	}
	return u, nil
}

// ReadLimited reads at most maxBytes+1 bytes from r via io.LimitReader
// (so a malicious/oversized response body is never fully buffered in
// memory - we bound the read itself, not just the slice afterward)
// and reports whether the body was truncated.
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
