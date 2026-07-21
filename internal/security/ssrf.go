// Package security provides outbound request hardening.
package security

import (
	"fmt"
	"net"
	"os"
	"strings"
)

// IsPrivateIP checks if an IP address is in private/internal ranges.
func IsPrivateIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	privateBlocks := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"169.254.0.0/16",
		"0.0.0.0/8",
	}
	for _, block := range privateBlocks {
		_, cidr, _ := net.ParseCIDR(block)
		if cidr != nil && cidr.Contains(ip) {
			return true
		}
	}
	return false
}

// AllowPrivateIPs checks the WEB_ALLOW_PRIVATE_IPS env var.
func AllowPrivateIPs() bool {
	return os.Getenv("WEB_ALLOW_PRIVATE_IPS") == "1"
}

// ValidateHost checks if a host is safe to connect to.
func ValidateHost(host string) error {
	if AllowPrivateIPs() {
		return nil
	}
	// Resolve all IPs for the host
	ips, err := net.LookupIP(host)
	if err != nil {
		return err
	}
	if len(ips) == 0 {
		return fmt.Errorf("no IPs resolved for host %q", host)
	}
	for _, ip := range ips {
		if IsPrivateIP(ip) {
			return fmt.Errorf("host %q resolves to private IP %s — blocked (use WEB_ALLOW_PRIVATE_IPS=1 to override)", host, ip)
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
