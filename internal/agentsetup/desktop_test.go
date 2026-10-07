package agentsetup

import (
	"strings"
	"testing"
)

func TestConfigPath(t *testing.T) {
	a := Agents["claude-desktop"]
	tests := []struct {
		name, goos, home, appdata string
		want                      string // "" means an error
	}{
		{name: "macOS", goos: "darwin", home: "/Users/alice",
			want: "/Users/alice/Library/Application Support/Claude/claude_desktop_config.json"},
		{name: "macOS ignores APPDATA", goos: "darwin", home: "/Users/alice/", appdata: `C:\x`,
			want: "/Users/alice/Library/Application Support/Claude/claude_desktop_config.json"},
		{name: "Windows", goos: "windows", home: `C:\Users\alice`, appdata: `C:\Users\alice\AppData\Roaming`,
			want: `C:\Users\alice\AppData\Roaming\Claude\claude_desktop_config.json`},
		{name: "Windows redirected APPDATA", goos: "windows", home: `C:\Users\alice`, appdata: `D:\profiles\alice\`,
			want: `D:\profiles\alice\Claude\claude_desktop_config.json`},
		{name: "Windows without APPDATA", goos: "windows", home: `C:\Users\alice`},
		{name: "macOS without home", goos: "darwin"},
		{name: "Linux", goos: "linux", home: "/home/alice"},
		{name: "FreeBSD", goos: "freebsd", home: "/home/alice"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := a.ConfigPath(tc.goos, tc.home, tc.appdata)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("ConfigPath(%s, %q, %q) = %q, want an error", tc.goos, tc.home, tc.appdata, got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("ConfigPath(%s, %q, %q) = %q, %v, want %q", tc.goos, tc.home, tc.appdata, got, err, tc.want)
			}
		})
	}
	// Only desktop apps have one.
	if _, err := Agents["claude-code"].ConfigPath("darwin", "/Users/alice", ""); err == nil {
		t.Error("claude-code has a desktop config path")
	}
}

func TestServerKey(t *testing.T) {
	tests := []struct{ root, want string }{
		{"/Users/alice/src/widget", "starfix-widget"},
		{"/Users/alice/src/widget/", "starfix-widget"},
		{`C:\Users\alice\src\Widget Shop`, "starfix-widget-shop"},
		{"/home/bob/My_Repo.v2", "starfix-my-repo-v2"},
		{"/home/bob/--x--", "starfix-x"},
		{"/home/bob/日本", "starfix-project"},
		{"/", "starfix-project"},
		{"/src/" + strings.Repeat("a", 80), "starfix-" + strings.Repeat("a", 40)},
	}
	for _, tc := range tests {
		if got := ServerKey(tc.root); got != tc.want {
			t.Errorf("ServerKey(%q) = %q, want %q", tc.root, got, tc.want)
		}
	}
}

// A desktop app has no working directory: the entry pins the project
// with -C and is keyed by it, so several projects share one file, each
// added and removed alone, around the person's own servers and keys.
func TestDesktopEntries(t *testing.T) {
	a := Agents["claude-desktop"]
	widget := DesktopEntry("/usr/local/bin/sfx", "/Users/alice/src/widget")
	gadget := DesktopEntry("/usr/local/bin/sfx", "/Users/alice/src/gadget")
	in := `{"globalShortcut": "Alt+Space", "mcpServers": {"files": {"command": "npx", "args": ["-y", "x"]}}, "z": [1, 2.50]}`

	out, res, err := a.Apply([]byte(in), widget)
	if err != nil || res != Added {
		t.Fatalf("Apply(widget) = %s, %v", res, err)
	}
	want := `{
  "globalShortcut": "Alt+Space",
  "mcpServers": {
    "files": {
      "command": "npx",
      "args": [
        "-y",
        "x"
      ]
    },
    "starfix-widget": {
      "command": "/usr/local/bin/sfx",
      "args": [
        "-C",
        "/Users/alice/src/widget",
        "mcp"
      ],
      "env": {
        "STARFIX_HARNESS": "claude-desktop"
      }
    }
  },
  "z": [
    1,
    2.50
  ]
}
`
	if string(out) != want {
		t.Fatalf("Apply(widget) =\n%s\nwant\n%s", out, want)
	}
	if again, res, err := a.Apply(out, widget); err != nil || res != Unchanged || string(again) != string(out) {
		t.Fatalf("second Apply(widget) = %s, %v", res, err)
	}
	both, res, err := a.Apply(out, gadget)
	if err != nil || res != Added || !strings.Contains(string(both), `"starfix-widget"`) {
		t.Fatalf("Apply(gadget) = %s, %v\n%s", res, err, both)
	}
	if !a.Registered(both, widget) || !a.Registered(both, gadget) || a.Registered(out, gadget) {
		t.Fatal("Registered disagrees with two projects")
	}
	if a.Registered(both, DefaultEntry) {
		t.Fatal("a desktop entry registers the plain starfix server")
	}
	if dir, ok := a.ProjectDir(both, widget.Server); !ok || dir != "/Users/alice/src/widget" {
		t.Fatalf("ProjectDir(widget) = %q, %v", dir, ok)
	}
	if _, ok := a.ProjectDir(both, "starfix-other"); ok {
		t.Fatal("ProjectDir found an entry that is not there")
	}

	tg := a.DesktopTarget("/Users/alice/Library/Application Support/Claude/claude_desktop_config.json")
	gone, res, err := tg.Remove(both, gadget)
	if err != nil || res != Removed || string(gone) != string(out) {
		t.Fatalf("Remove(gadget) = %s, %v\n%s\nwant\n%s", res, err, gone, out)
	}
	if again, res, err := tg.Remove(gone, gadget); err != nil || res != Unchanged || string(again) != string(gone) {
		t.Fatalf("second Remove(gadget) = %s, %v", res, err)
	}
	if s := tg.Snippet(widget); !tg.Registered([]byte(s), widget) || !strings.Contains(s, `"starfix-widget"`) {
		t.Fatalf("snippet does not register:\n%s", s)
	}
}

func TestDesktopAgent(t *testing.T) {
	a := Agents["claude-desktop"]
	if !a.Desktop || a.ManualMCP() || len(a.Targets(false)) != 0 || len(a.Targets(true)) != 0 {
		t.Fatalf("claude-desktop: %+v", a)
	}
	if strings.Contains(strings.Join(Hooks(), " "), "claude-desktop") {
		t.Fatal("claude-desktop has a hook")
	}
}
