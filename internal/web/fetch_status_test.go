package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchRejectsHTTPErrorStatus(t *testing.T) {
	t.Setenv("WEB_ALLOW_PRIVATE_IPS", "1") // httptest listens on 127.0.0.1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"status":"502 Bad Gateway","desc":"未知错误"}`))
	}))
	defer srv.Close()

	_, err := Fetch(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("Fetch() expected error for HTTP 502")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Fatalf("error = %v, want it to mention 502", err)
	}
}

func TestFetchSucceedsOnOK(t *testing.T) {
	t.Setenv("WEB_ALLOW_PRIVATE_IPS", "1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body><main><p>Hello from the article with enough content here.</p></main></body></html>`))
	}))
	defer srv.Close()

	result, err := Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if result.StatusCode != 200 {
		t.Fatalf("StatusCode = %d", result.StatusCode)
	}
	if !strings.Contains(result.Text, "Hello from the article") {
		t.Fatalf("Text = %q", result.Text)
	}
}
