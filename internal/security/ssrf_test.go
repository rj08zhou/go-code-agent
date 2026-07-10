package security

import (
	"net"
	"os"
	"testing"
)

func mustIP(t *testing.T, s string) net.IP {
	t.Helper()
	ip := net.ParseIP(s)
	if ip == nil {
		t.Fatalf("invalid test IP literal %q", s)
	}
	return ip
}

func TestIsBlockedIP_DefaultDenyList(t *testing.T) {
	os.Unsetenv("WEB_ALLOW_PRIVATE_IPS")

	blocked := []string{
		"127.0.0.1",       // loopback
		"127.0.0.53",      // loopback range
		"::1",             // IPv6 loopback
		"169.254.169.254", // cloud metadata (AWS/GCP/Azure IMDS) - the classic SSRF target
		"169.254.1.1",     // link-local
		"fe80::1",         // IPv6 link-local
		"10.0.0.1",        // RFC1918
		"10.255.255.255",
		"172.16.0.1",
		"172.31.255.255",
		"192.168.1.1",
		"192.168.255.255",
		"9.1.2.3",  // explicitly listed in project security rules
		"11.1.2.3", // explicitly listed
		"21.1.2.3", // explicitly listed
		"30.1.2.3", // explicitly listed
		"0.0.0.0",
		"fc00::1", // IPv6 ULA
	}
	for _, s := range blocked {
		ip := mustIP(t, s)
		if !IsBlockedIP(ip) {
			t.Errorf("IsBlockedIP(%s) = false, want true (must be blocked by default)", s)
		}
	}
}

func TestIsBlockedIP_PublicAddressesAllowed(t *testing.T) {
	os.Unsetenv("WEB_ALLOW_PRIVATE_IPS")

	allowed := []string{
		"8.8.8.8",              // Google DNS
		"1.1.1.1",              // Cloudflare DNS
		"93.184.216.34",        // example.com (a real public IP historically used by it)
		"140.82.112.3",         // github.com range
		"2606:4700:4700::1111", // Cloudflare IPv6 DNS
	}
	for _, s := range allowed {
		ip := mustIP(t, s)
		if IsBlockedIP(ip) {
			t.Errorf("IsBlockedIP(%s) = true, want false (public address must be reachable)", s)
		}
	}
}

func TestIsBlockedIP_NilFailsClosed(t *testing.T) {
	if !IsBlockedIP(nil) {
		t.Error("IsBlockedIP(nil) should fail closed (return true/blocked)")
	}
}

// TestIsBlockedIP_LinkLocalNeverAllowedEvenWithOverride is the core
// security invariant: WEB_ALLOW_PRIVATE_IPS=1 widens access to
// "private" ranges (including loopback, for reaching local dev
// services), but link-local / cloud-metadata addresses are in
// alwaysBlockedCIDRs and must stay blocked regardless - this is the
// address space real-world SSRF exploits (e.g. stealing cloud IAM
// credentials via 169.254.169.254) actually target, so it must never
// be a simple opt-out.
func TestIsBlockedIP_LinkLocalNeverAllowedEvenWithOverride(t *testing.T) {
	os.Setenv("WEB_ALLOW_PRIVATE_IPS", "1")
	defer os.Unsetenv("WEB_ALLOW_PRIVATE_IPS")

	mustStillBlock := []string{"169.254.169.254", "fe80::1"}
	for _, s := range mustStillBlock {
		ip := mustIP(t, s)
		if !IsBlockedIP(ip) {
			t.Errorf("IsBlockedIP(%s) = false even with override set, want true (link-local/metadata must never be allowed)", s)
		}
	}
}

// TestIsBlockedIP_OverrideAllowsGeneralPrivateRanges verifies the
// override widens access to loopback and RFC1918 space - the ranges
// an operator explicitly opting into "internal network access" would
// plausibly want (e.g. reaching a local dev server or an internal
// service), as opposed to the metadata endpoints tested above.
func TestIsBlockedIP_OverrideAllowsGeneralPrivateRanges(t *testing.T) {
	os.Setenv("WEB_ALLOW_PRIVATE_IPS", "1")
	defer os.Unsetenv("WEB_ALLOW_PRIVATE_IPS")

	nowAllowed := []string{"10.0.0.1", "192.168.1.1", "172.16.0.1", "9.1.2.3", "127.0.0.1", "::1"}
	for _, s := range nowAllowed {
		ip := mustIP(t, s)
		if IsBlockedIP(ip) {
			t.Errorf("IsBlockedIP(%s) = true with WEB_ALLOW_PRIVATE_IPS=1, want false", s)
		}
	}
}

func TestAllowPrivateNetworkAccess_Parsing(t *testing.T) {
	cases := map[string]bool{
		"":      false,
		"0":     false,
		"false": false,
		"no":    false,
		"1":     true,
		"true":  true,
		"YES":   true,
		"On":    true,
	}
	for v, want := range cases {
		os.Setenv("WEB_ALLOW_PRIVATE_IPS", v)
		if got := AllowPrivateNetworkAccess(); got != want {
			t.Errorf("AllowPrivateNetworkAccess() with env=%q = %v, want %v", v, got, want)
		}
	}
	os.Unsetenv("WEB_ALLOW_PRIVATE_IPS")
}

func TestCheckDialIP(t *testing.T) {
	os.Unsetenv("WEB_ALLOW_PRIVATE_IPS")
	if err := CheckDialIP(mustIP(t, "169.254.169.254")); err == nil {
		t.Error("CheckDialIP(metadata IP) should return an error")
	}
	if err := CheckDialIP(mustIP(t, "8.8.8.8")); err != nil {
		t.Errorf("CheckDialIP(public IP) should not error, got %v", err)
	}
}
