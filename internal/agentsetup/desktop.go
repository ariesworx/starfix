package agentsetup

import (
	"encoding/json"
	"errors"
	"strings"
)

// A desktop app (Claude Desktop) differs from the other harnesses in four
// ways that shape its setup. It keeps one MCP config per user, at a path
// that depends on the platform. It starts servers with no working
// directory, so `sfx mcp` cannot find .starfix.yaml by itself: the entry
// pins the project with -C. It starts them without the login shell's
// PATH, so the command is an absolute path. And it reads no instruction
// file and runs no hook, so the MCP server's instructions are all the
// agent is told.

// ErrNoDesktop is ConfigPath's error on a platform the app does not run
// on.
var ErrNoDesktop = errors.New("no desktop app on this platform")

// ConfigPath is the desktop app's config file on goos, given the home
// directory and, on Windows, %APPDATA%. The result uses goos's
// separator. Claude Desktop runs only on macOS and Windows.
func (a Agent) ConfigPath(goos, home, appdata string) (string, error) {
	if !a.Desktop {
		return "", errors.New(a.Title + " is not a desktop app")
	}
	switch goos {
	case "darwin":
		if home == "" {
			return "", errors.New("no home directory")
		}
		return strings.TrimRight(home, "/") + "/Library/Application Support/" + a.AppConfig, nil
	case "windows":
		if appdata == "" {
			return "", errors.New("APPDATA is not set")
		}
		return strings.TrimRight(appdata, `\/`) + `\` + strings.ReplaceAll(a.AppConfig, "/", `\`), nil
	}
	return "", ErrNoDesktop
}

// maxKeyName caps the project part of a ServerKey.
const maxKeyName = 40

// ServerKey is the name a project's desktop entry is registered under:
// ServerName, a hyphen and the project directory's name, lowercased,
// with each run of characters other than a-z and 0-9 made one hyphen.
// root may use either separator.
func ServerKey(root string) string {
	base := strings.TrimRight(root, `/\`)
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(base) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(r)
			dash = false
			continue
		}
		dash = true
	}
	name := b.String()
	if len(name) > maxKeyName {
		name = strings.TrimRight(name[:maxKeyName], "-")
	}
	if name == "" {
		name = "project"
	}
	return ServerName + "-" + name
}

// DesktopEntry starts `command -C root mcp` under root's ServerKey.
// command and root are absolute.
func DesktopEntry(command, root string) Entry {
	return Entry{Command: command, Args: []string{"-C", root, "mcp"}, Server: ServerKey(root)}
}

// DesktopTarget is the desktop app's one target: its MCP config at path,
// an absolute path from ConfigPath.
func (a Agent) DesktopTarget(path string) Target {
	return Target{Kind: KindMCP, Path: path, agent: a}
}

// ProjectDir is the directory the entry registered as server pins with
// -C; ok reports whether content has such an entry at all, and dir is ""
// when it pins none. Two checkouts with the same directory name share a
// ServerKey; setup compares dir with its own root before it replaces or
// removes an entry.
func (a Agent) ProjectDir(content []byte, server string) (dir string, ok bool) {
	entry, ok := jsonEntry(content, a.format, server)
	if !ok {
		return "", false
	}
	raw, _ := entry.get("args")
	var args []string
	_ = json.Unmarshal(raw, &args) // args that are not all strings pin nothing
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-C" {
			return args[i+1], true
		}
	}
	return "", true
}
