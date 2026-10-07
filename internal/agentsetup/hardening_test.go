package agentsetup

import (
	"strings"
	"testing"
)

// A shadowing duplicate key decides what the harness runs, since
// JSON.parse keeps the last one; setup refuses such a file rather than
// edit the first copy and call it registered (T-2). Inside the starfix
// entry a duplicate is not registered either, and Apply's wholesale
// replacement leaves one copy of each key.
func TestJSONRefusesDuplicateKeys(t *testing.T) {
	tests := []struct {
		name, in string
		refused  bool
	}{
		{"top level", `{"mcpServers":{},"mcpServers":{"starfix":{"command":"/tmp/evil","args":["mcp"]}}}`, true},
		{"servers", `{"mcpServers":{"starfix":{"command":"sfx","args":["mcp"]},"starfix":{"command":"/tmp/evil"}}}`, true},
		{"escaped twin", `{"mcpServers":{},"mcp\u0053ervers":{}}`, true},
		{"entry", `{"mcpServers":{"starfix":{"command":"sfx","args":["mcp"],"command":"/tmp/evil"}}}`, false},
		{"env", `{"mcpServers":{"starfix":{"command":"sfx","args":["mcp"],"env":{"STARFIX_HARNESS":"gemini","STARFIX_HARNESS":"x"}}}}`, false},
	}
	a := Agents["gemini"]
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if a.Registered([]byte(tc.in), DefaultEntry) {
				t.Errorf("Registered(%s) = true, want false", tc.in)
			}
			out, _, err := a.Apply([]byte(tc.in), DefaultEntry)
			if !tc.refused {
				if err != nil || string(out) != a.Snippet(DefaultEntry) {
					t.Errorf("Apply(%s) = %s, %v; want the snippet", tc.in, out, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "duplicate") {
				t.Errorf("Apply(%s) = %s, %v; want a duplicate-key error", tc.in, out, err)
			}
			if _, _, err := a.Remove([]byte(tc.in)); err == nil || !strings.Contains(err.Error(), "duplicate") {
				t.Errorf("Remove(%s) = %v; want a duplicate-key error", tc.in, err)
			}
		})
	}
}

// Text after the object makes the file unreadable to the harness, so
// setup refuses it rather than call it registered, or rewrite the file
// without it.
func TestJSONRefusesTextAfterTheObject(t *testing.T) {
	a := Agents["gemini"]
	tests := []struct{ name, in string }{
		{"a word", `{"mcpServers":{}} x`},
		{"a stray brace", `{"mcpServers":{}}}`},
		{"a stray bracket", `{"mcpServers":{}} ]`},
		{"a comment", "{\"mcpServers\":{}}\n// keep this\n"},
		{"a second object", `{"mcpServers":{}} {}`},
		{"a registered entry, then a brace", a.Snippet(DefaultEntry) + "}"},
	}
	const want = "not JSON: text after the object"
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if a.Registered([]byte(tc.in), DefaultEntry) {
				t.Errorf("Registered(%q) = true, want false", tc.in)
			}
			if out, res, err := a.Apply([]byte(tc.in), DefaultEntry); err == nil || err.Error() != want {
				t.Errorf("Apply(%q) = %q, %s, %v; want error %q", tc.in, out, res, err, want)
			}
			if out, res, err := a.Remove([]byte(tc.in)); err == nil || err.Error() != want {
				t.Errorf("Remove(%q) = %q, %s, %v; want error %q", tc.in, out, res, err, want)
			}
		})
	}
}

// An entry with the right command but extra keys runs something else:
// --check reports it, and Apply replaces the entry wholesale (C-8).
func TestRegisteredRefusesExtras(t *testing.T) {
	const good = `"command": "sfx", "args": ["mcp"]`
	tests := []struct {
		name, agent, in string
	}{
		{"json PATH", "gemini", `{"mcpServers": {"starfix": {` + good + `, "env": {"STARFIX_HARNESS": "gemini", "PATH": "./.tools:/usr/bin"}}}}`},
		{"json LD_PRELOAD", "claude-code", `{"mcpServers": {"starfix": {"type": "stdio", ` + good + `, "env": {"STARFIX_HARNESS": "claude-code", "LD_PRELOAD": "./x.so"}}}}`},
		{"json NODE_OPTIONS", "cursor", `{"mcpServers": {"starfix": {` + good + `, "env": {"STARFIX_HARNESS": "cursor", "NODE_OPTIONS": "--require ./x.js"}}}}`},
		{"json cwd", "gemini", `{"mcpServers": {"starfix": {` + good + `, "cwd": "./tools", "env": {"STARFIX_HARNESS": "gemini"}}}}`},
		{"json envFile", "vscode", `{"servers": {"starfix": {"type": "stdio", ` + good + `, "envFile": ".env", "env": {"STARFIX_HARNESS": "vscode"}}}}`},
		{"json session", "gemini", `{"mcpServers": {"starfix": {` + good + `, "env": {"STARFIX_HARNESS": "gemini", "STARFIX_SESSION": "x"}}}}`},
		{"toml env", "codex", "[mcp_servers.starfix]\ncommand = \"sfx\"\nargs = [\"mcp\"]\n\n[mcp_servers.starfix.env]\nSTARFIX_HARNESS = \"codex\"\nLD_PRELOAD = \"./x.so\"\n"},
		{"toml cwd", "codex", "[mcp_servers.starfix]\ncommand = \"sfx\"\nargs = [\"mcp\"]\ncwd = \"./tools\"\n\n[mcp_servers.starfix.env]\nSTARFIX_HARNESS = \"codex\"\n"},
		{"toml dotted env", "codex", "[mcp_servers.starfix]\ncommand = \"sfx\"\nargs = [\"mcp\"]\nenv.PATH = \"./.tools\"\n\n[mcp_servers.starfix.env]\nSTARFIX_HARNESS = \"codex\"\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := Agents[tc.agent]
			if a.Registered([]byte(tc.in), DefaultEntry) {
				t.Fatalf("Registered(%s) = true, want false", tc.in)
			}
			out, res, err := a.Apply([]byte(tc.in), DefaultEntry)
			if err != nil || res != Updated {
				t.Fatalf("Apply = %s, %v; want updated", res, err)
			}
			for _, extra := range []string{"PATH", "LD_PRELOAD", "NODE_OPTIONS", "cwd", "envFile", "STARFIX_SESSION"} {
				if strings.Contains(string(out), extra) {
					t.Errorf("Apply kept %s:\n%s", extra, out)
				}
			}
			if want := a.Snippet(DefaultEntry); string(out) != want {
				t.Errorf("Apply = \n%s\nwant the snippet\n%s", out, want)
			}
			if !a.Registered(out, DefaultEntry) {
				t.Errorf("Apply's output is not registered:\n%s", out)
			}
		})
	}
}

// The Codex editor understands TOML's strings, arrays and comments:
// valid TOML in gives valid TOML out with starfix registered, and a
// second run changes nothing (T-3, C-11).
func TestApplyTOMLStructure(t *testing.T) {
	const block = "[mcp_servers.starfix]\ncommand = \"sfx\"\nargs = [\"mcp\"]\n\n[mcp_servers.starfix.env]\nSTARFIX_HARNESS = \"codex\"\n"
	tests := []struct{ name, in, want string }{
		{"nested array",
			"[mcp_servers.starfix]\ncommand = \"old\"\nargs = [[\"a\"],\n[\"b\"]]\n",
			block},
		{"bracket in a string",
			"[mcp_servers.starfix]\nargs = [\"a]\",\n \"b\"]\n",
			block},
		{"header inside a basic multi-line string",
			"x = \"\"\"\n[mcp_servers.starfix]\n\"\"\"\n",
			"x = \"\"\"\n[mcp_servers.starfix]\n\"\"\"\n\n" + block},
		{"header inside a literal multi-line string",
			"x = '''\n[mcp_servers.starfix]\ncommand = \"evil\"\n'''\n",
			"x = '''\n[mcp_servers.starfix]\ncommand = \"evil\"\n'''\n\n" + block},
		{"escaped quotes in a multi-line string",
			"x = \"\"\"a\\\"\"\"\n[mcp_servers.starfix]\n\"\"\"\n",
			"x = \"\"\"a\\\"\"\"\n[mcp_servers.starfix]\n\"\"\"\n\n" + block},
		{"comment with a bracket in an array",
			"[mcp_servers.starfix]\nargs = [ # ]\n  \"serve\", # [\n]\nstartup_timeout_sec = 20\n\n[other]\ny = 2\n",
			block + "\n[other]\ny = 2\n"},
		{"keeps comments before the table and after it",
			"# servers\n[mcp_servers.\"starfix\"] # ours\ncommand = \"x\"\n# about other\n[other]\n",
			"# servers\n" + block + "\n# about other\n[other]\n"},
		{"env table elsewhere moves beside the table",
			"[mcp_servers.starfix.env]\nA = \"b\"\n\n[other]\ny = 2\n\n[mcp_servers.starfix]\ncommand = \"x\"\n",
			block + "\n[other]\ny = 2\n"},
		{"subtables are starfix's too",
			"[mcp_servers.starfix]\ncommand = \"x\"\n\n[mcp_servers.starfix.tools.a]\nz = 1\n",
			block},
		{"inline env is replaced",
			"[mcp_servers.starfix]\ncommand = \"sfx\"\nenv = { A = \"b\" }\n",
			block},
		{"other servers stay",
			"[mcp_servers.other]\ncommand = \"o\"\nargs = [\n  \"x\",\n]\n",
			"[mcp_servers.other]\ncommand = \"o\"\nargs = [\n  \"x\",\n]\n\n" + block},
		{"date time and inline table values",
			"when = 1979-05-27 07:32:00Z\npt = { x = 1, y = \"}\" }\n",
			"when = 1979-05-27 07:32:00Z\npt = { x = 1, y = \"}\" }\n\n" + block},
	}
	a := Agents["codex"]
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, _, err := a.Apply([]byte(tc.in), DefaultEntry)
			if err != nil {
				t.Fatalf("Apply(%q) = %v", tc.in, err)
			}
			if string(out) != tc.want {
				t.Fatalf("Apply(%q) =\n%s\nwant\n%s", tc.in, out, tc.want)
			}
			if _, err := scanTOML(out); err != nil {
				t.Fatalf("Apply's output does not scan: %v", err)
			}
			again, res, err := a.Apply(out, DefaultEntry)
			if err != nil || res != Unchanged || string(again) != string(out) {
				t.Fatalf("second Apply = %s, %v\n%s", res, err, again)
			}
			if !a.Registered(out, DefaultEntry) {
				t.Fatal("not registered after Apply")
			}
		})
	}
}

// What the editor cannot place safely, it refuses; setup then prints
// the snippet to add by hand.
func TestApplyTOMLRefuses(t *testing.T) {
	tests := []struct{ name, in string }{
		{"unterminated multi-line string", "x = \"\"\"\n[mcp_servers.starfix]\n"},
		{"unterminated array", "args = [\"a\",\n"},
		{"newline in a basic string", "x = \"a\nb\"\n"},
		{"starfix as an inline table", "[mcp_servers]\nstarfix = { command = \"evil\" }\n"},
		{"starfix by dotted keys", "mcp_servers.starfix.command = \"evil\"\n"},
		{"servers as an inline table", "mcp_servers = { other = { command = \"x\" } }\n"},
		{"servers as an array", "[[mcp_servers]]\nx = 1\n"},
		{"twice", "[mcp_servers.starfix]\ncommand = \"a\"\n[mcp_servers.starfix]\ncommand = \"b\"\n"},
		{"bad header", "[mcp_servers.starfix\n"},
		{"text after a value", "x = 1 2\n"},
		{"bad escape in a key", "\"a\\q\" = 1\n"},
	}
	a := Agents["codex"]
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if out, _, err := a.Apply([]byte(tc.in), DefaultEntry); err == nil {
				t.Errorf("Apply(%q) =\n%s\nwant an error", tc.in, out)
			}
			if a.Registered([]byte(tc.in), DefaultEntry) {
				t.Errorf("Registered(%q) = true", tc.in)
			}
		})
	}
	if _, _, err := a.Remove([]byte("x = \"\"\"\n[mcp_servers.starfix]\n")); err == nil {
		t.Error("Remove accepted an unterminated string")
	}
}

// TOML basic strings take only TOML's escapes; Go's \x.. and \a are not
// among them.
func TestTOMLString(t *testing.T) {
	tests := []struct{ in, want string }{
		{`C:\tools\sfx.exe`, `"C:\\tools\\sfx.exe"`},
		{"a\"b", `"a\"b"`},
		{"\a\x01\x7f", `"\u0007\u0001\u007F"`},
		{"tab\tnl\n", `"tab\tnl\n"`},
		{"é", `"é"`},
	}
	for _, tc := range tests {
		if got := tomlString(tc.in); got != tc.want {
			t.Errorf("tomlString(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

// A hook is starfix's only when its whole command is one program token
// running prime --hook: a person's compound command that ends the same
// way is theirs (C-10).
func TestOurHookExact(t *testing.T) {
	tests := []struct {
		cmd  string
		want bool
	}{
		{"sfx prime --hook", true},
		{"sfx prime --hook=codex", true},
		{"/usr/local/bin/sfx prime --hook", true},
		{"'/opt/star fix/sfx' prime --hook", true},
		{"'/opt/it'\\''s/sfx' prime --hook=gemini", true},
		{`C:\tools\sfx.exe prime --hook`, false}, // not shell-safe unquoted, so not what setup writes
		{`'C:\tools\sfx.exe' prime --hook`, true},
		{"make env; ./bin/sfx prime --hook", false},
		{"cd x && sfx prime --hook", false},
		{"$(curl x) sfx prime --hook", false},
		{"/tmp/evil sfx prime --hook", false},
		{"bd prime --hook", false},
		{"sfx prime --hook --x", false},
		{"'sfx prime --hook", false},
	}
	for _, tc := range tests {
		h := object{{"type", mustJSON("command")}, {"command", mustJSON(tc.cmd)}}
		if got := ourHook(h, DefaultEntry, "claude-code"); got != tc.want {
			t.Errorf("ourHook(%q) = %v, want %v", tc.cmd, got, tc.want)
		}
	}
	in := `{"hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "make env; ./bin/sfx prime --hook"}]}]}}`
	h := hookOf(t, "claude-code", false)
	if out, res, err := h.Remove([]byte(in), DefaultEntry); err != nil || res != Unchanged || string(out) != in {
		t.Errorf("Remove(compound hook) = %s, %v\n%s", res, err, out)
	}
	out, res, err := h.Apply([]byte(in), DefaultEntry)
	if err != nil || res != Added || !strings.Contains(string(out), "make env; ./bin/sfx prime --hook") {
		t.Errorf("Apply beside a compound hook = %s, %v\n%s", res, err, out)
	}
}

// FuzzApplyTOML: whatever the editor accepts, its output scans, holds
// the entry, and a second run changes nothing.
func FuzzApplyTOML(f *testing.F) {
	for _, s := range []string{"model = \"o3\"\n", "[mcp_servers.starfix]\ncommand = \"old\"\nargs = [\n  \"a\",\n  \"b\",\n]\n",
		"x = '''\n[mcp_servers.starfix]\n'''\n", "[mcp_servers.starfix.env]\nA = 1\n\n\n[b]\n", "a = [1, # ]\n 2]\r\n"} {
		f.Add([]byte(s))
	}
	a := Agents["codex"]
	f.Fuzz(func(t *testing.T, in []byte) {
		out, _, err := a.Apply(in, DefaultEntry)
		if err != nil {
			return
		}
		if _, err := scanTOML(out); err != nil {
			t.Fatalf("output does not scan: %v\nin %q\nout %q", err, in, out)
		}
		if !a.Registered(out, DefaultEntry) {
			t.Fatalf("not registered:\nin %q\nout %q", in, out)
		}
		if rm, _, err := a.Remove(out); err != nil || tomlHas(rm) {
			t.Fatalf("Remove(out) = %q, %v", rm, err)
		}
	})
}
