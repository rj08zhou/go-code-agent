package security

import (
	"net/url"
	"strings"
)

// SSRFNetworkChecker adapts the shared SSRF host rules to the tool executor's
// NetworkChecker interface. It is a preflight check; Web requests must still
// use the dial-time CheckDialIP guard.
type SSRFNetworkChecker struct{}

func NewSSRFNetworkChecker() *SSRFNetworkChecker {
	return &SSRFNetworkChecker{}
}

// AllowHost reports whether host is safe to use as a network destination.
func (SSRFNetworkChecker) AllowHost(host string) bool {
	return ValidateHost(strings.TrimSpace(host)) == nil
}

// AllowURL reports whether rawURL is a supported URL with a safe destination.
func (SSRFNetworkChecker) AllowURL(rawURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Hostname() == "" {
		return false
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return false
	}
	return SSRFNetworkChecker{}.AllowHost(parsed.Hostname())
}
