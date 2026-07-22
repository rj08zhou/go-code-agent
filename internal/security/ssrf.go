// Package security provides outbound request hardening.
package security

import (
	"fmt"
	"net"
	"os"
	"strings"
)

// alwaysBlockedCIDRs are denied unconditionally, even when
// WEB_ALLOW_PRIVATE_IPS=1 is set. Cloud metadata/IMDS lives in
// link-local space (169.254.169.254), so link-local never gets an opt-out.
var alwaysBlockedCIDRs = mustParseCIDRs([]string{
	"169.254.0.0/16", // link-local / cloud metadata
	"fe80::/10",      // IPv6 link-local
	"0.0.0.0/8",      // "this network" — never a valid destination
})

// privateCIDRs are denied by default but MAY be allowed by setting
// WEB_ALLOW_PRIVATE_IPS=1.
var privateCIDRs = mustParseCIDRs([]string{
	"127.0.0.0/8",    // loopback
	"::1/128",        // IPv6 loopback
	"10.0.0.0/8",     // RFC1918
	"172.16.0.0/12",  // RFC1918
	"192.168.0.0/16", // RFC1918
	"fc00::/7",       // IPv6 unique local
	"9.0.0.0/8",
	"11.0.0.0/8",
	"21.0.0.0/8",
	"30.0.0.0/8",
})

func mustParseCIDRs(cidrs []string) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic("security: invalid built-in CIDR " + c + ": " + err.Error())
		}
		nets = append(nets, n)
	}
	return nets
}

// AllowPrivateIPs reports whether WEB_ALLOW_PRIVATE_IPS opts into
// private/internal address space (still subject to always-blocked ranges).
func AllowPrivateIPs() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("WEB_ALLOW_PRIVATE_IPS"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// AllowPrivateNetworkAccess is an alias kept for callers that used the
// master-branch name.
func AllowPrivateNetworkAccess() bool { return AllowPrivateIPs() }

// IsPrivateIP reports whether ip is in a private/reserved range that is
// blocked by default (including always-blocked ranges). Prefer IsBlockedIP
// for dial-time decisions.
func IsPrivateIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	return IsBlockedIP(ip)
}

// IsBlockedIP reports whether ip must never be dialed by web tools:
// always-blocked ranges are denied even with WEB_ALLOW_PRIVATE_IPS=1;
// other private ranges are denied unless that override is set.
func IsBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	for _, n := range alwaysBlockedCIDRs {
		if n.Contains(ip) {
			return true
		}
	}
	if AllowPrivateIPs() {
		return false
	}
	for _, n := range privateCIDRs {
		if n.Contains(ip) {
			return true
		}
	}
	if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return true
	}
	return false
}

// CheckDialIP is the dial-time choke point: validate the resolved IP
// about to be connected, defeating DNS rebinding between check and connect.
func CheckDialIP(ip net.IP) error {
	if IsBlockedIP(ip) {
		return fmt.Errorf("blocked: %s is a private/reserved address (set WEB_ALLOW_PRIVATE_IPS=1 to allow internal network access; link-local/metadata remain blocked)", ip)
	}
	return nil
}

// ValidateHost checks whether a hostname resolves only to safe IPs.
func ValidateHost(host string) error {
	if host == "" {
		return fmt.Errorf("empty host")
	}
	// Strip port if present for host-only checks.
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if ip := net.ParseIP(host); ip != nil {
		if IsBlockedIP(ip) {
			return fmt.Errorf("host %q is a private/reserved address — blocked", host)
		}
		return nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return err
	}
	if len(ips) == 0 {
		return fmt.Errorf("no IPs resolved for host %q", host)
	}
	for _, ip := range ips {
		if IsBlockedIP(ip) {
			return fmt.Errorf("host %q resolves to private IP %s — blocked (use WEB_ALLOW_PRIVATE_IPS=1 to override; link-local/metadata remain blocked)", host, ip)
		}
	}
	return nil
}

// RedactSecretsInLog replaces known env secrets with ***.
func RedactSecretsInLog(s string, secrets ...string) string {
	for _, sec := range secrets {
		if sec == "" {
			continue
		}
		s = strings.ReplaceAll(s, sec, "***")
	}
	return s
}
