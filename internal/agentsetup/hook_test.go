package agentsetup

import (
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
    ]
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
    ]
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
    ]
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
			out, res, err := applyHook([]byte(tc.in), e)
			if err != nil || res != tc.result || string(out) != tc.want {
				t.Fatalf("apply: %s %v\n%s\nwant %s\n%s", res, err, out, tc.result, tc.want)
			}
			if again, res, err := applyHook(out, e); err != nil || res != Unchanged || string(again) != string(out) {
				t.Fatalf("second apply: %s %v\n%s", res, err, again)
			}
			if !hookRegistered(out, e) || hookRegistered([]byte(tc.in), e) {
				t.Fatal("hookRegistered disagrees")
			}
			rm, res, err := removeHook(out, e)
			if err != nil || res != Removed || string(rm) != tc.removed {
				t.Fatalf("remove: %s %v\n%s\nwant\n%s", res, err, rm, tc.removed)
			}
			if again, res, _ := removeHook(rm, e); res != Unchanged || string(again) != string(rm) {
				t.Fatalf("second remove: %s", res)
			}
		})
	}
}

func TestHookLeavesOthersAlone(t *testing.T) {
	// Another tool's prime hook is not starfix's.
	in := `{"hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "bd prime --hook"}]}]}}`
	if _, res, err := removeHook([]byte(in), DefaultEntry); err != nil || res != Unchanged {
		t.Fatalf("%s %v", res, err)
	}
	out, res, err := applyHook([]byte(in), DefaultEntry)
	if err != nil || res != Added || !strings.Contains(string(out), `"bd prime --hook"`) {
		t.Fatalf("%s %v\n%s", res, err, out)
	}
}

func TestHookRefusesBadJSON(t *testing.T) {
	for _, in := range []string{"[]", `{"hooks": []}`, `{"hooks": {"SessionStart": {}}}`, `{"hooks": {"SessionStart": [1]}}`,
		`{"hooks": {"SessionStart": [{"hooks": {}}]}}`} {
		if _, _, err := applyHook([]byte(in), DefaultEntry); err == nil {
			t.Errorf("%s accepted", in)
		}
	}
}

func TestHookCommand(t *testing.T) {
	tests := []struct{ cmd, want string }{
		{"sfx", "sfx prime --hook"},
		{"/usr/local/bin/sfx", "/usr/local/bin/sfx prime --hook"},
		{`C:\tools\sfx.exe`, `'C:\tools\sfx.exe' prime --hook`},
		{"it's", `'it'\''s' prime --hook`},
	}
	for _, tc := range tests {
		if got := HookCommand(Entry{Command: tc.cmd}); got != tc.want {
			t.Errorf("HookCommand(%q) = %q, want %q", tc.cmd, got, tc.want)
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
		{"codex", false, "mcp .codex/config.toml, pointer AGENTS.md"},
		{"codex", true, "mcp .codex/config.toml, pointer .codex/AGENTS.md"},
		{"gemini", false, "mcp .gemini/settings.json, pointer GEMINI.md"},
		{"gemini", true, "mcp .gemini/settings.json, pointer .gemini/GEMINI.md"},
		{"cursor", false, "mcp .cursor/mcp.json, pointer .cursor/rules/starfix.mdc"},
		{"cursor", true, "mcp .cursor/mcp.json"},
		{"vscode", false, "mcp .vscode/mcp.json, pointer .github/copilot-instructions.md"},
	}
	// VS Code's user MCP config is per platform: setup refuses --global.
	if a := Agents["vscode"]; a.Global != "" || a.NoGlobal == "" || len(a.Targets(true)) != 0 {
		t.Errorf("vscode global: %+v", a.Targets(true))
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
