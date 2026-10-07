package agentsetup

import (
	"strings"
	"testing"
)

func TestApply(t *testing.T) {
	custom := Entry{Command: `C:\tools\starfix.exe`, Args: []string{"mcp"}}
	tests := []struct {
		name, agent, in, want string
		entry                 Entry
		result                Result
	}{
		{name: "claude new file", agent: "claude-code", result: Added, want: `{
  "mcpServers": {
    "starfix": {
      "type": "stdio",
      "command": "starfix",
      "args": [
        "mcp"
      ]
    }
  }
}
`},
		{name: "claude keeps other servers and order", agent: "claude-code", result: Added,
			in: `{"z": 1.50, "mcpServers": {"figma": {"type": "http", "url": "https://mcp.example.com"}}, "a": true}`,
			want: `{
  "z": 1.50,
  "mcpServers": {
    "figma": {
      "type": "http",
      "url": "https://mcp.example.com"
    },
    "starfix": {
      "type": "stdio",
      "command": "starfix",
      "args": [
        "mcp"
      ]
    }
  },
  "a": true
}
`},
		{name: "claude updates and keeps env", agent: "claude-code", result: Updated, entry: custom,
			in: `{"mcpServers": {"starfix": {"command": "old", "env": {"STARFIX_SESSION": "x"}, "args": []}}}`,
			want: `{
  "mcpServers": {
    "starfix": {
      "command": "C:\\tools\\starfix.exe",
      "env": {
        "STARFIX_SESSION": "x"
      },
      "args": [
        "mcp"
      ],
      "type": "stdio"
    }
  }
}
`},
		{name: "gemini new file", agent: "gemini", result: Added, in: "\n", want: `{
  "mcpServers": {
    "starfix": {
      "command": "starfix",
      "args": [
        "mcp"
      ]
    }
  }
}
`},
		{name: "codex new file", agent: "codex", result: Added,
			want: "[mcp_servers.starfix]\ncommand = \"starfix\"\nargs = [\"mcp\"]\n"},
		{name: "codex appends after other tables", agent: "codex", result: Added,
			in:   "model = \"o4\"\n\n[mcp_servers.other]\ncommand = \"x\"\n",
			want: "model = \"o4\"\n\n[mcp_servers.other]\ncommand = \"x\"\n\n[mcp_servers.starfix]\ncommand = \"starfix\"\nargs = [\"mcp\"]\n"},
		{name: "codex updates in place, keeping comments and keys", agent: "codex", result: Updated, entry: custom,
			in:   "# top\n[mcp_servers.\"starfix\"] # ours\nargs = [\n  \"serve\",\n]\nstartup_timeout_sec = 20\n\n[mcp_servers.starfix.env]\nA = \"b\"\n",
			want: "# top\n[mcp_servers.\"starfix\"] # ours\ncommand = \"C:\\\\tools\\\\starfix.exe\"\nargs = [\"mcp\"]\nstartup_timeout_sec = 20\n\n[mcp_servers.starfix.env]\nA = \"b\"\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := Agents[tc.agent]
			e := tc.entry
			if e.Command == "" {
				e = DefaultEntry
			}
			out, res, err := a.Apply([]byte(tc.in), e)
			if err != nil {
				t.Fatal(err)
			}
			if res != tc.result || string(out) != tc.want {
				t.Fatalf("%s\n%s\nwant %s\n%s", res, out, tc.result, tc.want)
			}
			// Twice changes nothing, byte for byte.
			again, res, err := a.Apply(out, e)
			if err != nil || res != Unchanged || string(again) != string(out) {
				t.Fatalf("second apply: %s %v\n%s", res, err, again)
			}
			if !a.Registered(out, e) || a.Registered(out, Entry{Command: "other", Args: e.Args}) {
				t.Fatal("Registered disagrees")
			}
		})
	}
}

// An already-registered file is left exactly as it is, even when its
// formatting is not ours.
func TestApplyLeavesRegisteredFileAlone(t *testing.T) {
	in := `{"mcpServers":{"starfix":{"args":["mcp"],"command":"starfix","type":"stdio"}}}`
	out, res, err := Agents["claude-code"].Apply([]byte(in), DefaultEntry)
	if err != nil || res != Unchanged || string(out) != in {
		t.Fatalf("%s %v %s", res, err, out)
	}
}

func TestRemove(t *testing.T) {
	tests := []struct{ agent, in, want string }{
		{"claude-code", `{"mcpServers": {"a": {}, "starfix": {"command": "starfix"}}}`, "{\n  \"mcpServers\": {\n    \"a\": {}\n  }\n}\n"},
		{"codex", "x = 1\n\n[mcp_servers.starfix]\ncommand = \"starfix\"\n\n[mcp_servers.starfix.env]\nA = \"b\"\n\n[other]\ny = 2\n",
			"x = 1\n\n[other]\ny = 2\n"},
	}
	for _, tc := range tests {
		a := Agents[tc.agent]
		out, res, err := a.Remove([]byte(tc.in))
		if err != nil || res != Removed || string(out) != tc.want {
			t.Fatalf("%s: %s %v\n%s\nwant\n%s", tc.agent, res, err, out, tc.want)
		}
		if again, res, _ := a.Remove(out); res != Unchanged || string(again) != string(out) {
			t.Fatalf("%s: second remove %s", tc.agent, res)
		}
	}
}

func TestApplyRefusesBadJSON(t *testing.T) {
	for _, in := range []string{"[1]", "{", `{"mcpServers": []}`, "{} {}"} {
		if _, _, err := Agents["gemini"].Apply([]byte(in), DefaultEntry); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestSnippets(t *testing.T) {
	for _, name := range Names() {
		a := Agents[name]
		s := a.Snippet(DefaultEntry)
		if !strings.Contains(s, "starfix") || !strings.Contains(s, "mcp") {
			t.Errorf("%s snippet: %s", name, s)
		}
		if !a.Registered([]byte(s), DefaultEntry) {
			t.Errorf("%s: its own snippet does not register", name)
		}
	}
}
