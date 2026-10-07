package release

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// workflow is the part of a GitHub Actions workflow these tests read.
type workflow struct {
	On   map[string]any `yaml:"on"`
	Jobs map[string]struct {
		Uses  string            `yaml:"uses"`
		Needs any               `yaml:"needs"`
		Env   map[string]string `yaml:"env"`
		Steps []struct {
			Uses string `yaml:"uses"`
			Run  string `yaml:"run"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func readWorkflow(t *testing.T, name string) workflow {
	t.Helper()
	b, err := os.ReadFile("../../.github/workflows/" + name) //nolint:gosec // a fixed workflow name
	if err != nil {
		t.Fatal(err)
	}
	var w workflow
	if err := yaml.Unmarshal(b, &w); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return w
}

func needs(v any) []string {
	switch n := v.(type) {
	case string:
		return []string{n}
	case []any:
		var out []string
		for _, s := range n {
			if s, ok := s.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// TestReleaseRunsGate pins the release gate: the CI gate runs on the tagged
// commit, the build waits for it, and signing waits for the build.
func TestReleaseRunsGate(t *testing.T) {
	rel := readWorkflow(t, "release.yml")
	if got := rel.Jobs["gate"].Uses; got != "./.github/workflows/ci.yml" {
		t.Errorf("release.yml gate job uses %q, want ./.github/workflows/ci.yml", got)
	}
	if got := needs(rel.Jobs["build"].Needs); !slices.Contains(got, "gate") {
		t.Errorf("release.yml build needs %v, want gate", got)
	}
	if got := needs(rel.Jobs["sign-and-publish"].Needs); !slices.Contains(got, "build") {
		t.Errorf("release.yml sign-and-publish needs %v, want build", got)
	}
	ci := readWorkflow(t, "ci.yml")
	if _, ok := ci.On["workflow_call"]; !ok {
		t.Error("ci.yml cannot be called by release.yml: no workflow_call trigger")
	}
}

// TestCIRequiresDolt pins that CI runs the Dolt-backed tests rather than
// skipping them, in random order.
func TestCIRequiresDolt(t *testing.T) {
	test := readWorkflow(t, "ci.yml").Jobs["test"]
	if got := test.Env["STARFIX_REQUIRE_DOLT"]; got != "1" {
		t.Errorf("ci.yml test job STARFIX_REQUIRE_DOLT = %q, want 1", got)
	}
	var shuffled bool
	for _, s := range test.Steps {
		if strings.Contains(s.Run, "go test") && strings.Contains(s.Run, "./...") {
			shuffled = strings.Contains(s.Run, "-shuffle=on")
		}
	}
	if !shuffled {
		t.Error("ci.yml runs go test ./... without -shuffle=on")
	}
}

// TestActionsPinned checks AGENTS.md rule 4: every third-party action is
// pinned by full commit SHA, with a version comment.
func TestActionsPinned(t *testing.T) {
	pinned := regexp.MustCompile(`^\s*(?:-\s+)?uses:\s+[\w.-]+/[\w./-]+@[0-9a-f]{40} # v\S+\s*$`)
	uses := regexp.MustCompile(`^\s*(?:-\s+)?uses:\s+(\S+)`)
	files, err := os.ReadDir("../../.github/workflows")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		b, err := os.ReadFile("../../.github/workflows/" + f.Name())
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			m := uses.FindStringSubmatch(line)
			if m == nil || strings.HasPrefix(m[1], "./") {
				continue
			}
			if !pinned.MatchString(line) {
				t.Errorf("%s:%d: %q is not pinned by commit SHA with a version comment", f.Name(), i+1, strings.TrimSpace(line))
			}
		}
	}
}
