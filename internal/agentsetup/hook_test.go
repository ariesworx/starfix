package agentsetup

import (
	"slices"
	"strings"
	"testing"
)

func TestHook(t *testing.T) {
	custom := Entry{Command: "/opt/star fix/sfx", Args: []string{"mcp"}}
	tests := []struct {
		name, in, want string
		entry          Entry
		result         Result
		// removed is what Remove leaves of want.
		removed string
	}{
		{name: "new file", result: Added, removed: "{}\n", want: `{
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "sfx prime --hook"
          }
        ]
      }
    ],
` + usageHooks("sfx") + `
  }
}
`},
		{name: "keeps other settings and hooks", result: Added,
			in: `{"permissions": {"deny": ["Read(./.env)"]}, "hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "lint"}]}],
				"SessionStart": [{"matcher": "startup", "hooks": [{"type": "command", "command": "bd prime"}]}]}, "model": "x"}`,
			want: `{
  "permissions": {
    "deny": [
      "Read(./.env)"
    ]
  },
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": "lint"
          }
        ]
      }
    ],
    "SessionStart": [
      {
        "matcher": "startup",
        "hooks": [
          {
            "type": "command",
            "command": "bd prime"
          }
        ]
      },
      {
        "hooks": [
          {
            "type": "command",
            "command": "sfx prime --hook"
          }
        ]
      }
    ],
` + usageHooks("sfx") + `
  },
  "model": "x"
}
`,
			removed: `{
  "permissions": {
    "deny": [
      "Read(./.env)"
    ]
  },
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": "lint"
          }
        ]
      }
    ],
    "SessionStart": [
      {
        "matcher": "startup",
        "hooks": [
          {
            "type": "command",
            "command": "bd prime"
          }
        ]
      }
    ]
  },
  "model": "x"
}
`},
		{name: "updates the command in place, keeping its timeout", result: Updated, entry: custom,
			in: `{"hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "sfx prime --hook", "timeout": 20}, {"type": "command", "command": "other"}]}]}}`,
			want: `{
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "'/opt/star fix/sfx' prime --hook",
            "timeout": 20
          },
          {
            "type": "command",
            "command": "other"
          }
        ]
      }
    ],
` + usageHooks("'/opt/star fix/sfx'") + `
  }
}
`,
			removed: `{
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "other"
          }
        ]
      }
    ]
  }
}
`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := tc.entry
			if e.Command == "" {
				e = DefaultEntry
			}
			h := hookOf(t, "claude-code", false)
			out, res, err := h.Apply([]byte(tc.in), e)
			if err != nil || res != tc.result || string(out) != tc.want {
				t.Fatalf("apply: %s %v\n%s\nwant %s\n%s", res, err, out, tc.result, tc.want)
			}
			if again, res, err := h.Apply(out, e); err != nil || res != Unchanged || string(again) != string(out) {
				t.Fatalf("second apply: %s %v\n%s", res, err, again)
			}
			if !h.Registered(out, e) || h.Registered([]byte(tc.in), e) {
				t.Fatal("Registered disagrees")
			}
			rm, res, err := h.Remove(out, e)
			if err != nil || res != Removed || string(rm) != tc.removed {
				t.Fatalf("remove: %s %v\n%s\nwant\n%s", res, err, rm, tc.removed)
			}
			if again, res, _ := h.Remove(rm, e); res != Unchanged || string(again) != string(rm) {
				t.Fatalf("second remove: %s", res)
			}
		})
	}
}

// usageHooks is the Stop, SubagentStop and SessionEnd entries setup
// writes in Claude Code's settings for program, as they sit inside
// "hooks", with no trailing newline.
func usageHooks(program string) string {
	var parts []string
	for _, event := range []string{"Stop", "SubagentStop", "SessionEnd"} {
		parts = append(parts, `    "`+event+`": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "`+program+` usage --hook",
            "async": true,
            "timeout": 30
          }
        ]
      }
    ]`)
	}
	return strings.Join(parts, ",\n")
}

// An install from before usage capture has only the prime hook: it is
// not registered, says which hooks are missing, and a second Apply adds
// them, keeping the prime hook as the person left it.
func TestHookUpgradeAddsUsageHooks(t *testing.T) {
	old := `{"hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "sfx prime --hook", "timeout": 20}]}]}}`
	h := hookOf(t, "claude-code", false)
	if h.Registered([]byte(old), DefaultEntry) {
		t.Fatal("Registered with only the prime hook")
	}
	if got, want := h.MissingHooks([]byte(old), DefaultEntry), []string{"Stop", "SubagentStop", "SessionEnd"}; !slices.Equal(got, want) {
		t.Errorf("MissingHooks(prime only) = %v, want %v", got, want)
	}
	out, res, err := h.Apply([]byte(old), DefaultEntry)
	if err != nil || res != Updated || !h.Registered(out, DefaultEntry) || !strings.Contains(string(out), `"timeout": 20`) ||
		strings.Count(string(out), "sfx usage --hook") != 3 {
		t.Fatalf("Apply(prime only) = %s, %v\n%s\nwant updated, the usage hooks added and the prime hook kept", res, err, out)
	}
	if got := h.MissingHooks(out, DefaultEntry); len(got) != 0 {
		t.Errorf("MissingHooks after Apply = %v, want none", got)
	}
	if got := h.MissingHooks(nil, DefaultEntry); len(got) != 4 {
		t.Errorf("MissingHooks(new file) = %v, want all four", got)
	}
}

// Only Claude Code's usage is captured so far: Junie shares its hook
// format but gets the prime hook alone.
func TestUsageHooksAreClaudeCodes(t *testing.T) {
	out, _, err := hookOf(t, "junie", true).Apply(nil, DefaultEntry)
	if err != nil || strings.Contains(string(out), "usage") {
		t.Fatalf("junie hooks = %v\n%s\nwant no usage hook", err, out)
	}
	if got := strings.Join(UsageHooks(), " "); got != "claude-code" {
		t.Errorf("UsageHooks() = %s, want claude-code", got)
	}
}

func TestHookLeavesOthersAlone(t *testing.T) {
	// Another tool's prime hook is not starfix's.
	in := `{"hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "bd prime --hook"}]}]}}`
	h := hookOf(t, "claude-code", false)
	if _, res, err := h.Remove([]byte(in), DefaultEntry); err != nil || res != Unchanged {
		t.Fatalf("%s %v", res, err)
	}
	out, res, err := h.Apply([]byte(in), DefaultEntry)
	if err != nil || res != Added || !strings.Contains(string(out), `"bd prime --hook"`) {
		t.Fatalf("%s %v\n%s", res, err, out)
	}
}

func TestHookRefusesBadJSON(t *testing.T) {
	for _, in := range []string{"[]", `{"hooks": []}`, `{"hooks": {"SessionStart": {}}}`, `{"hooks": {"SessionStart": [1]}}`,
		`{"hooks": {"SessionStart": [{"hooks": {}}]}}`} {
		if _, _, err := hookOf(t, "claude-code", false).Apply([]byte(in), DefaultEntry); err == nil {
			t.Errorf("%s accepted", in)
		}
	}
}

func TestHookCommand(t *testing.T) {
	tests := []struct{ cmd, harness, want string }{
		{"sfx", "claude-code", "sfx prime --hook"},
		{"/usr/local/bin/sfx", "claude-code", "/usr/local/bin/sfx prime --hook"},
		{`C:\tools\sfx.exe`, "claude-code", `'C:\tools\sfx.exe' prime --hook`},
		{"it's", "claude-code", `'it'\''s' prime --hook`},
		{"sfx", "gemini", "sfx prime --hook=gemini"},
		{"/opt/star fix/sfx", "cursor", "'/opt/star fix/sfx' prime --hook=cursor"},
	}
	for _, tc := range tests {
		if got := HookCommand(Entry{Command: tc.cmd}, tc.harness); got != tc.want {
			t.Errorf("HookCommand(%q, %q) = %q, want %q", tc.cmd, tc.harness, got, tc.want)
		}
	}
}

func TestTargets(t *testing.T) {
	tests := []struct {
		agent  string
		global bool
		want   string
	}{
		{"claude-code", false, "mcp .mcp.json, pointer CLAUDE.md, hook .claude/settings.json"},
		{"claude-code", true, "mcp .claude.json, pointer .claude/CLAUDE.md, hook .claude/settings.json"},
		{"codex", false, "mcp .codex/config.toml, pointer AGENTS.md, hook .codex/hooks.json"},
		{"codex", true, "mcp .codex/config.toml, pointer .codex/AGENTS.md, hook .codex/hooks.json"},
		{"gemini", false, "mcp .gemini/settings.json, pointer GEMINI.md, hook .gemini/settings.json"},
		{"gemini", true, "mcp .gemini/settings.json, pointer .gemini/GEMINI.md, hook .gemini/settings.json"},
		{"cursor", false, "mcp .cursor/mcp.json, pointer .cursor/rules/starfix.mdc, hook .cursor/hooks.json"},
		{"cursor", true, "mcp .cursor/mcp.json, hook .cursor/hooks.json"},
		{"vscode", false, "mcp .vscode/mcp.json, pointer .github/copilot-instructions.md, hook .github/hooks/starfix.json"},
		// Junie ignores hooks in a project's config: only the home one gets it.
		{"junie", false, "mcp .junie/mcp/mcp.json, pointer AGENTS.md"},
		{"junie", true, "mcp .junie/mcp/mcp.json, pointer .junie/AGENTS.md, hook .junie/config.json"},
		// AI Assistant keeps its MCP servers in IDE settings, not a file.
		{"jetbrains", false, "pointer .aiassistant/rules/starfix.md"},
	}
	// VS Code's user MCP config is per platform, and AI Assistant has no
	// file at all: setup refuses --global for both.
	for _, name := range []string{"vscode", "jetbrains"} {
		if a := Agents[name]; a.Global != "" || a.NoGlobal == "" || len(a.Targets(true)) != 0 {
			t.Errorf("%s global: %+v", name, a.Targets(true))
		}
	}
	for _, tc := range tests {
		var got []string
		for _, tg := range Agents[tc.agent].Targets(tc.global) {
			got = append(got, tg.Kind.String()+" "+tg.Path)
		}
		if s := strings.Join(got, ", "); s != tc.want {
			t.Errorf("%s global=%v: %s, want %s", tc.agent, tc.global, s, tc.want)
		}
	}
}

func TestTargetSnippetsRegister(t *testing.T) {
	for _, name := range Names() {
		for _, tg := range Agents[name].Targets(false) {
			if s := tg.Snippet(DefaultEntry); !tg.Registered([]byte(s), DefaultEntry) {
				t.Errorf("%s %s: its own snippet does not register:\n%s", name, tg.Kind, s)
			}
		}
	}
}
