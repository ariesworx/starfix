// Package agentspec_test keeps the repository's agent specifications in
// step. Claude Code and Gemini CLI read Markdown with YAML front matter;
// Codex reads TOML. Each carries the same prompt, so this test fails when
// one is edited and the others are not.
package agentspec_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// specs names the agents; each has a file under .claude, .gemini and .codex.
var specs = []string{"go-engineer"}

type spec struct {
	name, description, body string
}

// repo opens the repository root, confining reads to it.
func repo(t *testing.T) *os.Root {
	t.Helper()
	r, err := os.OpenRoot(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// markdown splits a front-matter file into its fields and its body.
func markdown(t *testing.T, r *os.Root, path string) spec {
	t.Helper()
	b, err := r.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rest, ok := strings.CutPrefix(string(b), "---\n")
	if !ok {
		t.Fatalf("%s: no front matter", path)
	}
	front, body, ok := strings.Cut(rest, "\n---\n\n")
	if !ok {
		t.Fatalf("%s: front matter not closed by ---", path)
	}
	var f struct{ Name, Description string }
	if err := yaml.Unmarshal([]byte(front), &f); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return spec{f.Name, f.Description, body}
}

// codex reads the fields this test needs from a Codex agent file. It
// expects one `key = "value"` per line and developer_instructions as a
// multi-line literal string (three single quotes), as these files are written.
func codex(t *testing.T, r *os.Root, path string) spec {
	t.Helper()
	b, err := r.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	head, rest, ok := strings.Cut(string(b), "developer_instructions = '''\n")
	if !ok {
		t.Fatalf("%s: no developer_instructions = ''' literal", path)
	}
	body, ok := strings.CutSuffix(rest, "'''\n")
	if !ok || strings.Contains(body, "'''") {
		t.Fatalf("%s: developer_instructions must end the file with '''", path)
	}
	var s spec
	s.body = body
	for line := range strings.Lines(head) {
		k, v, ok := strings.Cut(strings.TrimSpace(line), " = ")
		if !ok {
			continue
		}
		v = strings.Trim(v, `"`)
		switch k {
		case "name":
			s.name = v
		case "description":
			s.description = v
		}
	}
	return s
}

func TestSpecsAgree(t *testing.T) {
	r := repo(t)
	for _, name := range specs {
		t.Run(name, func(t *testing.T) {
			claude := markdown(t, r, ".claude/agents/"+name+".md")
			if claude.name != name || claude.description == "" || claude.body == "" {
				t.Fatalf("Claude spec: name %q, description %q, %d-byte body", claude.name, claude.description, len(claude.body))
			}
			for path, got := range map[string]spec{
				".gemini/agents/" + name + ".md":  markdown(t, r, ".gemini/agents/"+name+".md"),
				".codex/agents/" + name + ".toml": codex(t, r, ".codex/agents/"+name+".toml"),
			} {
				if got.name != claude.name {
					t.Errorf("%s: name = %q, want %q", path, got.name, claude.name)
				}
				if got.description != claude.description {
					t.Errorf("%s: description differs from .claude/agents/%s.md", path, name)
				}
				if got.body != claude.body {
					t.Errorf("%s: prompt differs from .claude/agents/%s.md; copy the body across", path, name)
				}
			}
		})
	}
}
