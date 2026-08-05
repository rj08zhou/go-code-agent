package application

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-code-agent/internal/config"
	"go-code-agent/internal/event"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/model"
	"go-code-agent/internal/model/provider"
	"go-code-agent/internal/session"
)

type ctorStubProvider struct{}

func (ctorStubProvider) Name() string { return "openai" }
func (ctorStubProvider) Capabilities() model.ProviderCapabilities {
	return model.ProviderCapabilities{}
}
func (ctorStubProvider) Call(context.Context, llm.CallParams) (*llm.Completion, error) {
	return &llm.Completion{Content: "ok", FinishReason: "stop"}, nil
}
func (ctorStubProvider) Stream(context.Context, llm.CallParams, model.StreamSink) (*llm.StreamResult, error) {
	return &llm.StreamResult{Content: "ok", FinishReason: "stop"}, nil
}

func TestNewWithGatewayBuildsInteractiveIOFromReader(t *testing.T) {
	reg := provider.NewRegistry()
	reg.Register(ctorStubProvider{})
	calls := 0
	app, err := NewWithGateway(
		t.TempDir(),
		t.TempDir(),
		&config.Config{ModelID: "gpt-test", OpenAIAPIKey: "x"},
		model.NewGateway(ctorStubProvider{}, model.NewRoleThrottle(1)),
		reg,
		WithInteractiveReader(func(string) (string, error) {
			calls++
			return "y", nil
		}, true),
	)
	if err != nil {
		t.Fatal(err)
	}
	if app.consoleSink == nil {
		t.Fatal("expected Application to own a ConsoleSink")
	}
	io := app.interactive()
	if io == nil || !io.IsTTY() {
		t.Fatal("expected TTY InteractiveIO built from injected reader")
	}
	got, err := io.ReadLine("?")
	if err != nil || got != "y" || calls != 1 {
		t.Fatalf("ReadLine = (%q, %v), calls=%d", got, err, calls)
	}
}

func TestNewWithGatewayUsesInjectedConsoleSinkForInteractiveIO(t *testing.T) {
	sink := event.NewConsoleSink()
	reg := provider.NewRegistry()
	reg.Register(ctorStubProvider{})
	app, err := NewWithGateway(
		t.TempDir(),
		t.TempDir(),
		&config.Config{ModelID: "gpt-test", OpenAIAPIKey: "x"},
		model.NewGateway(ctorStubProvider{}, model.NewRoleThrottle(1)),
		reg,
		WithConsoleSink(sink),
		WithInteractiveReader(func(string) (string, error) { return "n", nil }, false),
	)
	if err != nil {
		t.Fatal(err)
	}
	if app.consoleSink != sink {
		t.Fatal("console sink was not injected")
	}
	// Same writer identity is not exposed; ensure headless TTY flag and reader work.
	if app.interactive().IsTTY() {
		t.Fatal("expected non-TTY InteractiveIO")
	}
}

func TestCloseSessionReleasesRuntimeAndPreservesCloseError(t *testing.T) {
	runtime := NewSessionRuntime(context.Background(), nil, "", nil, nil, &session.State{ID: "close-error"})
	calls := 0
	runtime.AddHook("failing", func(context.Context) error {
		calls++
		return errors.New("close failed")
	})
	app := &Application{runtime: runtime}

	first := app.CloseSession(context.Background())
	if first == nil || !strings.Contains(first.Error(), "close failed") {
		t.Fatalf("first CloseSession error = %v, want close failure", first)
	}
	if app.runtime != nil {
		t.Fatal("CloseSession retained the closed runtime")
	}
	second := app.Shutdown(context.Background())
	if second == nil || second.Error() != first.Error() {
		t.Fatalf("second Shutdown error = %v, want %v", second, first)
	}
	if calls != 1 {
		t.Fatalf("shutdown hook calls = %d, want 1", calls)
	}
}
