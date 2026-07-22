package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseDescription(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "inline",
			in:   "---\nname: foo\ndescription: A short skill. Use when needed.\n---\n# Body\n",
			want: "A short skill. Use when needed.",
		},
		{
			name: "block scalar",
			in:   "---\nname: bar\ndescription: |\n  Line one.\n  Line two.\n---\nBody",
			want: "Line one. Line two.",
		},
		{
			name: "no frontmatter",
			in:   "# Just a heading\nsome text",
			want: "",
		},
		{
			name: "missing description",
			in:   "---\nname: baz\n---\nBody",
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseDescription(c.in); got != c.want {
				t.Fatalf("parseDescription() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestSummariesUsesDescriptionsNotFullBody(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "alpha", "---\nname: alpha\ndescription: Alpha does things.\n---\nSECRET_BODY_ALPHA")
	writeSkill(t, dir, "beta", "---\nname: beta\ndescription: |\n  Beta helps.\n---\nSECRET_BODY_BETA")

	l := NewLoader(dir)
	sum := l.Summaries()

	for _, want := range []string{"alpha", "Alpha does things.", "beta", "Beta helps."} {
		if !strings.Contains(sum, want) {
			t.Errorf("Summaries() missing %q; got:\n%s", want, sum)
		}
	}
	// The full body must NOT leak into the compact catalog.
	for _, forbidden := range []string{"SECRET_BODY_ALPHA", "SECRET_BODY_BETA"} {
		if strings.Contains(sum, forbidden) {
			t.Errorf("Summaries() leaked full body %q", forbidden)
		}
	}
	// load_skill still returns the full body on demand.
	if !strings.Contains(l.Load("alpha"), "SECRET_BODY_ALPHA") {
		t.Errorf("Load(alpha) should return full body")
	}
}

func writeSkill(t *testing.T, root, name, content string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
