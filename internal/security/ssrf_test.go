package security

import (
	"net"
	"testing"
)

func TestIsBlockedIP_AlwaysBlockedEvenWithPrivateOverride(t *testing.T) {
	t.Setenv("WEB_ALLOW_PRIVATE_IPS", "1")
	for _, raw := range []string{"169.254.169.254", "169.254.0.1", "0.0.0.1"} {
		ip := net.ParseIP(raw)
		if !IsBlockedIP(ip) {
			t.Fatalf("%s must remain blocked when WEB_ALLOW_PRIVATE_IPS=1", raw)
		}
		if err := CheckDialIP(ip); err == nil {
			t.Fatalf("CheckDialIP(%s) should error", raw)
		}
	}
}

func TestIsBlockedIP_PrivateAllowedWithOverride(t *testing.T) {
	t.Setenv("WEB_ALLOW_PRIVATE_IPS", "1")
	for _, raw := range []string{"10.0.0.1", "192.168.1.1", "127.0.0.1"} {
		ip := net.ParseIP(raw)
		if IsBlockedIP(ip) {
			t.Fatalf("%s should be allowed when WEB_ALLOW_PRIVATE_IPS=1", raw)
		}
	}
}

func TestIsBlockedIP_PrivateDeniedByDefault(t *testing.T) {
	t.Setenv("WEB_ALLOW_PRIVATE_IPS", "")
	for _, raw := range []string{"10.0.0.1", "192.168.1.1", "127.0.0.1", "169.254.169.254"} {
		ip := net.ParseIP(raw)
		if !IsBlockedIP(ip) {
			t.Fatalf("%s should be blocked by default", raw)
		}
	}
}

func TestAllowPrivateIPs_TruthyValues(t *testing.T) {
	for _, v := range []string{"1", "true", "YES", "on"} {
		t.Setenv("WEB_ALLOW_PRIVATE_IPS", v)
		if !AllowPrivateIPs() {
			t.Fatalf("AllowPrivateIPs(%q) = false, want true", v)
		}
	}
	t.Setenv("WEB_ALLOW_PRIVATE_IPS", "0")
	if AllowPrivateIPs() {
		t.Fatal("AllowPrivateIPs(0) should be false")
	}
}
