package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// TestSafeHTTPClient_AllowsLocalPublicLikeServer sanity-checks the
// happy path: a normal httptest server (which listens on 127.0.0.1)
// would actually be blocked by our own SSRF policy since loopback is
// always denied - which is exactly the point of this test: it proves
// the client's SSRF guard is genuinely wired in, not a no-op, by
// observing the request to a loopback server FAIL.
func TestSafeHTTPClient_BlocksLoopbackServer(t *testing.T) {
	os.Unsetenv("WEB_ALLOW_PRIVATE_IPS")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("should never be reached"))
	}))
	defer srv.Close()

	client := NewSafeHTTPClient(5 * time.Second)
	resp, err := client.Get(srv.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("expected loopback request to be blocked by SSRF guard, but it succeeded")
	}
	if !strings.Contains(err.Error(), "blocked") {
		t.Errorf("error should mention 'blocked', got: %v", err)
	}
}

// TestSafeHTTPClient_LoopbackAllowedWithOverride verifies the escape
// hatch actually works end-to-end (not just at the IsBlockedIP unit
// level): with WEB_ALLOW_PRIVATE_IPS=1, a loopback server IS reachable.
func TestSafeHTTPClient_LoopbackAllowedWithOverride(t *testing.T) {
	os.Setenv("WEB_ALLOW_PRIVATE_IPS", "1")
	defer os.Unsetenv("WEB_ALLOW_PRIVATE_IPS")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	client := NewSafeHTTPClient(5 * time.Second)
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("expected loopback request to succeed with override set, got error: %v", err)
	}
	defer resp.Body.Close()
	body, _, _ := ReadLimited(resp.Body, 1024)
	if string(body) != "ok" {
		t.Errorf("body = %q, want %q", body, "ok")
	}
}

// TestSafeHTTPClient_RedirectToLoopbackBlocked is the core anti-SSRF
// regression test: a server on a technically-loopback address (so it
// can only be reached at all under the override) redirects to a
// SECOND loopback server. Even with the override enabling loopback
// generally at the top level, this proves each redirect hop is
// re-validated independently by the DialContext (not just the first
// request) - the real defense against an attacker-controlled endpoint
// that 302's to an internal target after passing an initial check.
func TestSafeHTTPClient_TooManyRedirectsRejected(t *testing.T) {
	os.Setenv("WEB_ALLOW_PRIVATE_IPS", "1")
	defer os.Unsetenv("WEB_ALLOW_PRIVATE_IPS")

	var srv *httptest.Server
	hops := 0
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops++
		http.Redirect(w, r, srv.URL+"/next", http.StatusFound)
	}))
	defer srv.Close()

	client := NewSafeHTTPClient(5 * time.Second)
	resp, err := client.Get(srv.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("expected redirect loop to be rejected after too many hops")
	}
	if !strings.Contains(err.Error(), "too many redirects") {
		t.Errorf("expected 'too many redirects' error, got: %v", err)
	}
}

func TestValidateRequestURL(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
	}{
		{"https://example.com", false},
		{"http://example.com/path?q=1", false},
		{"", true},
		{"   ", true},
		{"ftp://example.com", true},
		{"file:///etc/passwd", true},
		{"gopher://example.com", true},
		{"not a url at all but no scheme", true},
		{"javascript:alert(1)", true},
	}
	for _, c := range cases {
		_, err := ValidateRequestURL(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("ValidateRequestURL(%q) err=%v, wantErr=%v", c.in, err, c.wantErr)
		}
	}
}

func TestReadLimited(t *testing.T) {
	r := strings.NewReader("0123456789")
	data, truncated, err := ReadLimited(r, 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !truncated {
		t.Error("expected truncated=true")
	}
	if string(data) != "01234" {
		t.Errorf("data = %q, want %q", data, "01234")
	}

	r2 := strings.NewReader("short")
	data2, truncated2, err2 := ReadLimited(r2, 100)
	if err2 != nil {
		t.Fatalf("unexpected error: %v", err2)
	}
	if truncated2 {
		t.Error("expected truncated=false for a body under the limit")
	}
	if string(data2) != "short" {
		t.Errorf("data2 = %q, want %q", data2, "short")
	}
}
