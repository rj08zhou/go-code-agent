package security

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBashPolicyUsesInjectedPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "permissions.json")
	if err := os.WriteFile(path, []byte(`[{"tool":"bash","level":"block"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	perms := NewPermissions()
	if err := perms.Load(dir); err != nil {
		t.Fatal(err)
	}
	p := NewDefaultBashPolicy()
	allowed, _, reason := p.Validate("ls", perms)
	if allowed {
		t.Fatalf("expected block via injected perms, reason=%q", reason)
	}
	allowed, _, _ = p.Validate("ls", nil)
	if !allowed {
		t.Fatal("nil perms must not block allowlisted ls")
	}
}

func TestPermissionsLoadBareArray(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "permissions.json")
	if err := os.WriteFile(path, []byte(`[
		{"tool":"bash","level":"block"},
		{"tool":"write_file","level":"confirm"}
	]`), 0o644); err != nil {
		t.Fatal(err)
	}
	p := NewPermissions()
	if err := p.Load(dir); err != nil {
		t.Fatal(err)
	}
	if p.Match("bash", "ls") != "block" {
		t.Fatalf("bash match=%q, want block", p.Match("bash", "ls"))
	}
	if p.Match("write_file", `{"path":"x"}`) != "confirm" {
		t.Fatalf("write_file match=%q, want confirm", p.Match("write_file", "{}"))
	}
}

func TestPermissionsLoadMasterWrappedForm(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "permissions.json")
	if err := os.WriteFile(path, []byte(`{
		"rules": [
			{"tool":"bash","pattern":"rm*","action":"deny"},
			{"tool":"mcp__*","action":"ask"}
		]
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	p := NewPermissions()
	if err := p.Load(dir); err != nil {
		t.Fatal(err)
	}
	if got := p.Match("bash", "rm -rf tmp"); got != "block" {
		t.Fatalf("deny→block mapping: got %q", got)
	}
	if got := p.Match("bash", "ls"); got != "" {
		t.Fatalf("non-matching args should miss, got %q", got)
	}
	if got := p.Match("mcp__demo__tool", "{}"); got != "confirm" {
		t.Fatalf("ask→confirm for mcp glob: got %q", got)
	}
}
