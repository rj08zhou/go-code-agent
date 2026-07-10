package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// withPrivateAccess is a test helper: web_fetch's own SSRF guard would
// otherwise block every httptest server (they listen on 127.0.0.1),
// so tests that just want to exercise Fetch's HTTP/extraction logic
// (not the SSRF guard itself, which has its own dedicated tests in
// client_test.go) opt into loopback access for their duration.
func withPrivateAccess(t *testing.T) {
	t.Helper()
	os.Setenv("WEB_ALLOW_PRIVATE_IPS", "1")
	t.Cleanup(func() { os.Unsetenv("WEB_ALLOW_PRIVATE_IPS") })
}

func TestFetch_HTMLPage(t *testing.T) {
	withPrivateAccess(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<html><body><h1>Hi</h1><p>Some content here.</p></body></html>`))
	}))
	defer srv.Close()

	res, err := Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}
	if res.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", res.StatusCode)
	}
	if !strings.Contains(res.Text, "Hi") || !strings.Contains(res.Text, "Some content here.") {
		t.Errorf("extracted text missing expected content: %q", res.Text)
	}
	if res.Truncated {
		t.Error("small body should not be marked truncated")
	}
}

func TestFetch_PlainText(t *testing.T) {
	withPrivateAccess(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("just plain text, no tags"))
	}))
	defer srv.Close()

	res, err := Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}
	if res.Text != "just plain text, no tags" {
		t.Errorf("Text = %q, want passthrough of plain text", res.Text)
	}
}

func TestFetch_BinaryContentTypeNotDisplayed(t *testing.T) {
	withPrivateAccess(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte{0x89, 0x50, 0x4e, 0x47})
	}))
	defer srv.Close()

	res, err := Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}
	if !strings.Contains(res.Text, "non-text content") {
		t.Errorf("expected placeholder for binary content, got: %q", res.Text)
	}
}

func TestFetch_TruncatesOversizedBody(t *testing.T) {
	withPrivateAccess(t)
	big := strings.Repeat("x", FetchMaxBytes+1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(big))
	}))
	defer srv.Close()

	res, err := Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}
	if !res.Truncated {
		t.Error("expected Truncated=true for an oversized body")
	}
	if len(res.Text) > FetchMaxBytes {
		t.Errorf("Text length %d exceeds FetchMaxBytes %d", len(res.Text), FetchMaxBytes)
	}
}

func TestFetch_RejectsBadURL(t *testing.T) {
	if _, err := Fetch(context.Background(), "not-a-url"); err == nil {
		t.Error("expected error for invalid url")
	}
	if _, err := Fetch(context.Background(), "ftp://example.com"); err == nil {
		t.Error("expected error for disallowed scheme")
	}
}

// TestFetch_BlockedByDefaultOnLoopback is the SSRF check at the
// Fetch()-level (not just the underlying client): without the
// WEB_ALLOW_PRIVATE_IPS override, fetching a loopback server must fail.
func TestFetch_BlockedByDefaultOnLoopback(t *testing.T) {
	os.Unsetenv("WEB_ALLOW_PRIVATE_IPS")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("should not be reached"))
	}))
	defer srv.Close()

	if _, err := Fetch(context.Background(), srv.URL); err == nil {
		t.Error("expected Fetch to be blocked by SSRF guard for a loopback URL")
	}
}
