package agentsetup

import (
	"encoding/json"
	"strings"
	"testing"
)

// hookOf is the agent's hook target.
func hookOf(t *testing.T, agent string, global bool) Target {
	t.Helper()
	for _, tg := range Agents[agent].Targets(global) {
		if tg.Kind == KindHook {
			return tg
		}
	}
	t.Fatalf("%s global=%v has no hook target", agent, global)
	return Target{}
}

// Each harness gets the hook in its own format. Applying twice changes
// nothing; a new command updates the hook in place; removing gives the
// file back without it; another tool's prime hook is never starfix's.
func TestHookFormats(t *testing.T) {
	tests := []struct {
		agent  string
		global bool
		// want is a new file; removed is what Remove leaves of it, ""
		// meaning the file is starfix's own and may be deleted.
		want, removed string
	}{
		{agent: "gemini", removed: "{}\n", want: `{
  "hooks": {
    "SessionStart": [
      {
        "matcher": "startup",
        "hooks": [
          {
            "type": "command",
            "command": "sfx prime --hook=gemini",
            "timeout": 15000
          }
        ]
      },
      {
        "matcher": "resume",
        "hooks": [
          {
            "type": "command",
            "command": "sfx prime --hook=gemini",
            "timeout": 15000
          }
        ]
      },
      {
        "matcher": "clear",
        "hooks": [
          {
            "type": "command",
            "command": "sfx prime --hook=gemini",
            "timeout": 15000
          }
        ]
      }
    ]
  }
}
`},
		{agent: "codex", removed: "{}\n", want: `{
  "hooks": {
    "SessionStart": [
      {
        "matcher": "startup|resume|clear|compact",
        "hooks": [
          {
            "type": "command",
            "command": "sfx prime --hook=codex"
          }
        ]
      }
    ]
  }
}
`},
		{agent: "cursor", removed: "{\n  \"version\": 1\n}\n", want: `{
  "version": 1,
  "hooks": {
    "sessionStart": [
      {
        "command": "sfx prime --hook=cursor",
        "timeout": 30
      }
    ]
  }
}
`},
		{agent: "vscode", removed: "", want: `{
  "hooks": {
    "SessionStart": [
      {
        "type": "command",
        "command": "sfx prime --hook=vscode",
        "timeout": 15
      }
    ]
  }
}
`},
		{agent: "junie", global: true, removed: "{}\n", want: `{
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "sfx prime --hook=junie"
          }
        ]
      }
    ]
  }
}
`},
	}
	for _, tc := range tests {
		t.Run(tc.agent, func(t *testing.T) {
			h := hookOf(t, tc.agent, tc.global)
			out, res, err := h.Apply(nil, DefaultEntry)
			if err != nil || res != Added || string(out) != tc.want {
				t.Fatalf("Apply(new) = %s, %v\n%s\nwant added\n%s", res, err, out, tc.want)
			}
			if again, res, err := h.Apply(out, DefaultEntry); err != nil || res != Unchanged || string(again) != string(out) {
				t.Fatalf("second Apply = %s, %v\n%s", res, err, again)
			}
			if !h.Registered(out, DefaultEntry) || h.Registered(nil, DefaultEntry) ||
				h.Registered(out, Entry{Command: "/opt/sfx"}) {
				t.Fatal("Registered disagrees")
			}
			if s := h.Snippet(DefaultEntry); s != tc.want {
				t.Errorf("Snippet = %s, want the new file", s)
			}
			rm, res, err := h.Remove(out, DefaultEntry)
			if err != nil || res != Removed || string(rm) != tc.removed {
				t.Fatalf("Remove = %s, %v, %q; want removed, %q", res, err, rm, tc.removed)
			}
			if again, res, _ := h.Remove(rm, DefaultEntry); res != Unchanged || string(again) != string(rm) {
				t.Fatalf("second Remove = %s", res)
			}

			moved := Entry{Command: "/opt/star fix/sfx"}
			up, res, err := h.Apply(out, moved)
			if err != nil || res != Updated || !h.Registered(up, moved) || h.Registered(up, DefaultEntry) {
				t.Fatalf("Apply(moved) = %s, %v\n%s", res, err, up)
			}
			if got, want := strings.Count(string(up), "prime --hook"), strings.Count(tc.want, "prime --hook"); got != want {
				t.Fatalf("Apply(moved) left %d hooks, want %d:\n%s", got, want, up)
			}

			other := strings.ReplaceAll(tc.want, "sfx prime", "bd prime")
			if _, res, err := h.Remove([]byte(other), DefaultEntry); err != nil || res != Unchanged {
				t.Fatalf("Remove(another tool's hook) = %s, %v", res, err)
			}
			both, res, err := h.Apply([]byte(other), DefaultEntry)
			if err != nil || res != Added || !strings.Contains(string(both), "bd prime") {
				t.Fatalf("Apply beside another tool's hook = %s, %v\n%s", res, err, both)
			}
			if rm, _, _ := h.Remove(both, DefaultEntry); !strings.Contains(string(rm), "bd prime") || strings.Contains(string(rm), "sfx prime") {
				t.Fatalf("Remove took the other hook, or left its own:\n%s", rm)
			}
		})
	}
}

// Gemini matches SessionStart sources exactly, so starfix keeps one group
// per source; a missing group is added back and the others are kept.
func TestGeminiHookAddsMissingSource(t *testing.T) {
	h := hookOf(t, "gemini", false)
	in := `{"hooks": {"SessionStart": [{"matcher": "startup", "hooks": [{"type": "command", "command": "sfx prime --hook=gemini", "timeout": 5000}]}]}}`
	if h.Registered([]byte(in), DefaultEntry) {
		t.Fatal("Registered with only the startup group")
	}
	out, res, err := h.Apply([]byte(in), DefaultEntry)
	if err != nil || res != Updated || strings.Count(string(out), `"matcher"`) != 3 ||
		strings.Count(string(out), `"timeout": 5000`) != 1 {
		t.Fatalf("Apply = %s, %v\n%s", res, err, out)
	}
}

// Gemini keeps its MCP servers and hooks in one settings file; each part
// leaves the other alone.
func TestGeminiSettingsHoldBoth(t *testing.T) {
	a := Agents["gemini"]
	var content []byte
	for _, tg := range a.Targets(false) {
		if tg.Path != ".gemini/settings.json" {
			continue
		}
		out, _, err := tg.Apply(content, DefaultEntry)
		if err != nil {
			t.Fatal(err)
		}
		content = out
	}
	for _, tg := range a.Targets(false) {
		if tg.Path == ".gemini/settings.json" && !tg.Registered(content, DefaultEntry) {
			t.Errorf("%s not registered in\n%s", tg.Kind, content)
		}
	}
}

func TestHookOutput(t *testing.T) {
	const claude = `{"hookSpecificOutput":{"additionalContext":"hi\n","hookEventName":"SessionStart"}}`
	tests := []struct{ agent, want string }{
		{"claude-code", claude},
		{"gemini", claude},
		{"codex", claude},
		{"vscode", claude},
		{"junie", claude},
		{"cursor", `{"additional_context":"hi\n"}`},
	}
	for _, tc := range tests {
		b, err := json.Marshal(Agents[tc.agent].HookOutput("hi\n"))
		if err != nil || string(b) != tc.want {
			t.Errorf("%s: HookOutput = %s, %v; want %s", tc.agent, b, err, tc.want)
		}
	}
}

// Hooks lists the agents `prime --hook=` accepts: those setup writes a
// hook for.
func TestHooks(t *testing.T) {
	want := "claude-code codex cursor gemini junie vscode"
	if got := strings.Join(Hooks(), " "); got != want {
		t.Fatalf("Hooks() = %s, want %s", got, want)
	}
}

func TestJunieMCP(t *testing.T) {
	out, res, err := Agents["junie"].Apply(nil, DefaultEntry)
	want := `{
  "mcpServers": {
    "starfix": {
      "command": "sfx",
      "args": [
        "mcp"
      ],
      "env": {
        "STARFIX_HARNESS": "junie"
      }
    }
  }
}
`
	if err != nil || res != Added || string(out) != want {
		t.Fatalf("Apply = %s, %v\n%s", res, err, out)
	}
}

func TestJetBrainsRule(t *testing.T) {
	a := Agents["jetbrains"]
	rule := a.Targets(false)[0]
	out, res, err := rule.Apply(nil, DefaultEntry)
	if err != nil || res != Added || !strings.HasPrefix(string(out), "---\napply: always\n---\n\n") ||
		!strings.HasSuffix(string(out), block) {
		t.Fatalf("new rule: %s, %v\n%s", res, err, out)
	}
	if again, res, _ := rule.Apply(out, DefaultEntry); res != Unchanged || string(again) != string(out) {
		t.Fatalf("second apply: %s", res)
	}
	if rm, res, err := rule.Remove(out, DefaultEntry); err != nil || res != Removed || len(rm) != 0 {
		t.Fatalf("remove: %s, %v, %q", res, err, rm)
	}
	// The MCP server is added by hand: the steps and the JSON to paste.
	steps := a.Steps(DefaultEntry)
	for _, w := range []string{"Settings › Tools › AI Assistant › Model Context Protocol (MCP)", "Project", `"STARFIX_HARNESS": "jetbrains"`} {
		if !strings.Contains(steps, w) {
			t.Errorf("Steps lack %q:\n%s", w, steps)
		}
	}
}
