package security

import "testing"

func TestSSRFNetworkCheckerBlocksReservedDestinations(t *testing.T) {
	checker := NewSSRFNetworkChecker()
	for _, host := range []string{
		"127.0.0.1",
		"10.0.0.1",
		"169.254.169.254",
		"::1",
	} {
		if checker.AllowHost(host) {
			t.Errorf("AllowHost(%q) = true, want false", host)
		}
	}
}

func TestSSRFNetworkCheckerAllowsPublicIP(t *testing.T) {
	checker := NewSSRFNetworkChecker()
	if !checker.AllowHost("8.8.8.8") {
		t.Fatal("AllowHost(public IP) = false, want true")
	}
	if !checker.AllowURL("https://8.8.8.8/health") {
		t.Fatal("AllowURL(public HTTPS URL) = false, want true")
	}
}

func TestSSRFNetworkCheckerRejectsUnsupportedURLs(t *testing.T) {
	checker := NewSSRFNetworkChecker()
	for _, rawURL := range []string{
		"",
		"file:///etc/passwd",
		"ftp://8.8.8.8/file",
		"https://",
		"not a URL",
	} {
		if checker.AllowURL(rawURL) {
			t.Errorf("AllowURL(%q) = true, want false", rawURL)
		}
	}
}

func TestSSRFNetworkCheckerHonorsPrivateIPOverride(t *testing.T) {
	t.Setenv("WEB_ALLOW_PRIVATE_IPS", "1")
	checker := NewSSRFNetworkChecker()
	if !checker.AllowHost("127.0.0.1") {
		t.Fatal("private IP override should allow loopback preflight")
	}
	if checker.AllowHost("169.254.169.254") {
		t.Fatal("private IP override must not allow metadata address")
	}
}
