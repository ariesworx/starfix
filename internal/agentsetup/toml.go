package agentsetup

import (
	"regexp"
	"strconv"
	"strings"
)

// Codex's config is TOML. Rather than take a TOML library for one table,
// these edit the text: the [mcp_servers.starfix] table's command and args
// lines are replaced or added, and everything else, comments included, is
// left as it was.

var (
	headerRE  = regexp.MustCompile(`^\s*\[`)
	ourHeader = regexp.MustCompile(`^\s*\[\s*mcp_servers\s*\.\s*("starfix"|'starfix'|starfix)\s*\]\s*(#.*)?$`)
	ourSub    = regexp.MustCompile(`^\s*\[\s*mcp_servers\s*\.\s*("starfix"|'starfix'|starfix)\s*\.`)
	keyRE     = regexp.MustCompile(`^\s*(command|args)\s*=`)
)

// tomlString quotes s as a TOML basic string. Go's quoting agrees with
// TOML's for every printable string, which commands and arguments are.
func tomlString(s string) string { return strconv.Quote(s) }

func tomlLines(e Entry) (command, args string) {
	quoted := make([]string, len(e.Args))
	for i, a := range e.Args {
		quoted[i] = tomlString(a)
	}
	return "command = " + tomlString(e.Command), "args = [" + strings.Join(quoted, ", ") + "]"
}

func tomlTable(e Entry) []string {
	c, a := tomlLines(e)
	return []string{"[mcp_servers." + ServerName + "]", c, a}
}

// tomlSection finds the starfix table: its header line and the line after
// its last key (the next header, or the end).
func tomlSection(lines []string) (start, end int, ok bool) {
	for i, l := range lines {
		if !ourHeader.MatchString(l) {
			continue
		}
		end = i + 1
		for end < len(lines) && !headerRE.MatchString(lines[end]) {
			end++
		}
		return i, end, true
	}
	return 0, 0, false
}

// splitLines splits content into lines without the final newline's empty
// tail.
func splitLines(content []byte) []string {
	s := strings.TrimSuffix(string(content), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func joinLines(lines []string) []byte {
	if len(lines) == 0 {
		return nil
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

func applyTOML(content []byte, e Entry) []byte {
	lines := splitLines(content)
	start, end, ok := tomlSection(lines)
	if !ok {
		if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
			lines = append(lines, "")
		}
		return joinLines(append(lines, tomlTable(e)...))
	}
	c, a := tomlLines(e)
	want := map[string]string{"command": c, "args": a}
	section := []string{lines[start]}
	inArray := false // inside a multi-line value being replaced
	for _, l := range lines[start+1 : end] {
		if inArray {
			inArray = !strings.Contains(l, "]")
			continue
		}
		if m := keyRE.FindStringSubmatch(l); m != nil {
			v := l[strings.Index(l, "=")+1:]
			inArray = strings.Contains(v, "[") && !strings.Contains(v, "]")
			if w, pending := want[m[1]]; pending {
				section = append(section, w)
				delete(want, m[1])
			}
			continue // replaced above, or a duplicate dropped
		}
		section = append(section, l)
	}
	// Keys the table lacked go straight after the header.
	var missing []string
	for _, k := range []string{"command", "args"} {
		if w, ok := want[k]; ok {
			missing = append(missing, w)
		}
	}
	section = append(section[:1], append(missing, section[1:]...)...)
	out := append(append(append([]string{}, lines[:start]...), section...), lines[end:]...)
	return joinLines(out)
}

func removeTOML(content []byte) []byte {
	lines := splitLines(content)
	var out []string
	skip := false
	for _, l := range lines {
		if headerRE.MatchString(l) {
			skip = ourHeader.MatchString(l) || ourSub.MatchString(l)
		}
		if !skip {
			out = append(out, l)
		}
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return joinLines(out)
}

func tomlRegistered(content []byte, e Entry) bool {
	lines := splitLines(content)
	start, end, ok := tomlSection(lines)
	if !ok {
		return false
	}
	c, a := tomlLines(e)
	var gotC, gotA bool
	for _, l := range lines[start+1 : end] {
		switch strings.TrimSpace(l) {
		case c:
			gotC = true
		case a:
			gotA = true
		}
	}
	return gotC && gotA
}
