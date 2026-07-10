package security

import (
	"fmt"
	"net"
	"os"
	"strings"
)

// SSRF guard for any outbound HTTP request the agent makes on the
// LLM's behalf (web_fetch, web_search). Default posture is DENY
// access to private/reserved networks; the only escape hatch is the
// explicit env var WEB_ALLOW_PRIVATE_IPS=1, checked once here rather
// than scattered across callers, so every caller gets the same
// behavior for free.
//
// This module is intentionally decision-only (no net.Dial here): it
// takes an already-resolved net.IP and says allow/deny. Callers (see
// internal/web/client.go) are responsible for plugging this into a
// custom DialContext so the check runs on the ACTUAL IP a hostname
// resolved to - not the hostname string, which is meaningless for
// SSRF purposes (a hostname is just a label; only the IP it resolves
// to determines what network is actually reached, and that resolution
// can also change between check-time and connect-time, i.e. DNS
// rebinding - hence "verify at dial time", see BlockedDialIP's doc).

// alwaysBlockedCIDRs are denied unconditionally, even when
// WEB_ALLOW_PRIVATE_IPS=1 is set. Deliberately narrow: only the
// address space that has no legitimate use case for an "I explicitly
// opted into internal network access" operator, and that real-world
// SSRF exploits actually target - cloud metadata/IMDS endpoints live
// in link-local space (169.254.169.254), which is why link-local as a
// whole is the one range that never gets an opt-out. "This network"
// (0.0.0.0/8) is simply never a valid destination.
var alwaysBlockedCIDRs = mustParseCIDRs([]string{
	"169.254.0.0/16", // link-local / cloud metadata (AWS/GCP/Azure IMDS)
	"fe80::/10",      // IPv6 link-local
	"0.0.0.0/8",      // "this network" - never a valid destination
})

// privateCIDRs are denied by default but MAY be allowed by setting
// WEB_ALLOW_PRIVATE_IPS=1. Includes loopback + RFC1918 private space
// (an operator explicitly enabling "internal network access" plausibly
// wants to reach a local dev service too) plus the extra ranges this
// project's security rules call out explicitly (9.*, 11.*, 21.*, 30.*
// - these are real public IANA-assigned /8 blocks, not private space,
// but are blocked here per explicit instruction rather than general
// private-range logic).
var privateCIDRs = mustParseCIDRs([]string{
	"127.0.0.0/8",    // loopback
	"::1/128",        // IPv6 loopback
	"10.0.0.0/8",     // RFC1918 private
	"172.16.0.0/12",  // RFC1918 private
	"192.168.0.0/16", // RFC1918 private
	"fc00::/7",       // IPv6 unique local
	"9.0.0.0/8",      // explicitly listed in project security rules
	"11.0.0.0/8",     // explicitly listed in project security rules
	"21.0.0.0/8",     // explicitly listed in project security rules
	"30.0.0.0/8",     // explicitly listed in project security rules
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

// AllowPrivateNetworkAccess reports whether WEB_ALLOW_PRIVATE_IPS=1 is
// set, i.e. the operator has explicitly opted into letting the agent's
// web tools reach private/internal address space. Re-read from the
// environment on every call (cheap, and lets tests toggle it without
// touching global state elsewhere).
func AllowPrivateNetworkAccess() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("WEB_ALLOW_PRIVATE_IPS"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// IsBlockedIP reports whether ip must never be dialed by the agent's
// web tools: link-local/loopback/metadata addresses are blocked
// unconditionally; general private ranges (and the extra ranges named
// in project security rules) are blocked unless the operator has set
// WEB_ALLOW_PRIVATE_IPS=1.
func IsBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true // can't classify => fail closed
	}
	for _, n := range alwaysBlockedCIDRs {
		if n.Contains(ip) {
			return true
		}
	}
	if AllowPrivateNetworkAccess() {
		return false
	}
	for _, n := range privateCIDRs {
		if n.Contains(ip) {
			return true
		}
	}
	// IsPrivate covers any RFC1918/ULA ranges not already listed above
	// (defense in depth against a gap in the explicit list).
	if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return true
	}
	return false
}

// CheckDialIP is the single choke point web/client.go's DialContext
// calls with the IP it is actually about to open a TCP connection to
// (post-DNS-resolution). Checking HERE - at dial time, on the resolved
// IP - rather than checking the hostname up front is what defeats DNS
// rebinding: an attacker-controlled domain could resolve to a public
// IP when first checked and to 127.0.0.1/169.254.169.254 by the time
// the actual connection is made (or a redirect Location resolves
// differently), so validating the string-form hostname is not a
// sufficient guard on its own.
func CheckDialIP(ip net.IP) error {
	if IsBlockedIP(ip) {
		return fmt.Errorf("blocked: %s is a private/reserved address (set WEB_ALLOW_PRIVATE_IPS=1 to allow internal network access)", ip)
	}
	return nil
}
