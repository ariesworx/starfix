package agentsetup

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Codex's config is TOML. Rather than take a TOML library for one table,
// setup scans the file's structure: table headers and key/value pairs,
// with every kind of string, multi-line arrays, inline tables and
// comments, so that a header inside a string or a bracket inside an
// array is never mistaken for structure. It does not check values
// beyond that. The starfix entry ([mcp_servers.starfix], its env table
// and any other subtable) is replaced wholesale, at the place of the
// first of them; everything else, comments included, is left as it was.
// A file the scanner cannot read, or one that defines starfix in a form
// it does not edit (an inline table, dotted keys), is refused, and setup
// prints the snippet to add by hand.

// tomlItem is one statement of a TOML file: a table header, a key/value
// pair (possibly over several lines), or a blank or comment line.
type tomlItem struct {
	kind       tomlKind
	start, end int      // byte offsets; end is past the line's newline
	path       []string // a header's table, or a pair's full key
	array      bool     // a [[header]]
}

type tomlKind int

const (
	tomlBlank  tomlKind = iota // empty or whitespace only
	tomlNote                   // a comment line
	tomlHeader                 // [table] or [[array]]
	tomlPair                   // key = value
)

// tomlScanner reads a document's statements.
type tomlScanner struct {
	s   string
	pos int
}

func (sc *tomlScanner) errf(format string, a ...any) error {
	line := 1 + strings.Count(sc.s[:min(sc.pos, len(sc.s))], "\n")
	return fmt.Errorf("TOML line %d: %s", line, fmt.Sprintf(format, a...))
}

func (sc *tomlScanner) peek(n int) string {
	if sc.pos+n > len(sc.s) {
		return sc.s[sc.pos:]
	}
	return sc.s[sc.pos : sc.pos+n]
}

func (sc *tomlScanner) eof() bool { return sc.pos >= len(sc.s) }

func (sc *tomlScanner) skipSpace() {
	for !sc.eof() && (sc.s[sc.pos] == ' ' || sc.s[sc.pos] == '\t') {
		sc.pos++
	}
}

// eol consumes a newline (LF or CRLF), reporting whether there was one.
func (sc *tomlScanner) eol() bool {
	switch {
	case sc.peek(1) == "\n":
		sc.pos++
	case sc.peek(2) == "\r\n":
		sc.pos += 2
	default:
		return false
	}
	return true
}

// comment consumes a comment to the end of its line, newline excluded.
func (sc *tomlScanner) comment() error {
	for !sc.eof() && sc.s[sc.pos] != '\n' {
		c := sc.s[sc.pos]
		if c == '\r' && sc.peek(2) == "\r\n" {
			return nil
		}
		if c < 0x20 && c != '\t' || c == 0x7f {
			return sc.errf("control character in a comment")
		}
		sc.pos++
	}
	return nil
}

// endLine consumes optional space and comment, then a newline or the
// end of the document.
func (sc *tomlScanner) endLine() error {
	sc.skipSpace()
	if sc.peek(1) == "#" {
		if err := sc.comment(); err != nil {
			return err
		}
	}
	if sc.eof() || sc.eol() {
		return nil
	}
	return sc.errf("unexpected %q", sc.peek(1))
}

// gap skips space, newlines and comments inside an array or inline
// table.
func (sc *tomlScanner) gap() error {
	for {
		sc.skipSpace()
		switch {
		case sc.peek(1) == "#":
			if err := sc.comment(); err != nil {
				return err
			}
		case sc.eol():
		default:
			return nil
		}
	}
}

func bareKeyByte(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

// key reads a dotted key.
func (sc *tomlScanner) key() ([]string, error) {
	var path []string
	for {
		sc.skipSpace()
		var part string
		switch c := sc.peek(1); {
		case c == `"`:
			if sc.peek(3) == `"""` {
				return nil, sc.errf("a multi-line string cannot be a key")
			}
			raw, err := sc.basic()
			if err != nil {
				return nil, err
			}
			if part, err = unquoteBasic(raw); err != nil {
				return nil, sc.errf("key %s: %v", raw, err)
			}
		case c == "'":
			if sc.peek(3) == "'''" {
				return nil, sc.errf("a multi-line string cannot be a key")
			}
			raw, err := sc.literal()
			if err != nil {
				return nil, err
			}
			part = raw[1 : len(raw)-1]
		case c != "" && bareKeyByte(c[0]):
			start := sc.pos
			for !sc.eof() && bareKeyByte(sc.s[sc.pos]) {
				sc.pos++
			}
			part = sc.s[start:sc.pos]
		default:
			return nil, sc.errf("expected a key, found %q", c)
		}
		path = append(path, part)
		sc.skipSpace()
		if sc.peek(1) != "." {
			return path, nil
		}
		sc.pos++
	}
}

// basic reads a one-line basic string, quotes included.
func (sc *tomlScanner) basic() (string, error) {
	start := sc.pos
	sc.pos++
	for !sc.eof() {
		switch sc.s[sc.pos] {
		case '"':
			sc.pos++
			return sc.s[start:sc.pos], nil
		case '\\':
			sc.pos += 2
		case '\n', '\r':
			return "", sc.errf("newline in a string")
		default:
			sc.pos++
		}
	}
	return "", sc.errf("unterminated string")
}

// literal reads a one-line literal string, quotes included.
func (sc *tomlScanner) literal() (string, error) {
	start := sc.pos
	end := strings.IndexAny(sc.s[sc.pos+1:], "'\n")
	if end < 0 || sc.s[sc.pos+1+end] != '\'' {
		return "", sc.errf("unterminated string")
	}
	sc.pos += end + 2
	return sc.s[start:sc.pos], nil
}

// multiline reads a multi-line string whose delimiter is q (""" or ”').
// Up to two more quote characters may end its content.
func (sc *tomlScanner) multiline(q string) error {
	sc.pos += 3
	for !sc.eof() {
		if q == `"""` && sc.s[sc.pos] == '\\' {
			sc.pos += 2
			continue
		}
		if sc.peek(3) == q {
			sc.pos += 3
			for i := 0; i < 2 && sc.peek(1) == q[:1]; i++ {
				sc.pos++
			}
			return nil
		}
		sc.pos++
	}
	return sc.errf("unterminated multi-line string")
}

// value reads one value: a string, an array, an inline table or a
// scalar (number, boolean, date or time).
func (sc *tomlScanner) value() error {
	switch c := sc.peek(1); c {
	case `"`:
		if sc.peek(3) == `"""` {
			return sc.multiline(`"""`)
		}
		_, err := sc.basic()
		return err
	case "'":
		if sc.peek(3) == "'''" {
			return sc.multiline("'''")
		}
		_, err := sc.literal()
		return err
	case "[":
		sc.pos++
		for {
			if err := sc.gap(); err != nil {
				return err
			}
			if sc.peek(1) == "]" {
				sc.pos++
				return nil
			}
			if err := sc.value(); err != nil {
				return err
			}
			if err := sc.gap(); err != nil {
				return err
			}
			switch sc.peek(1) {
			case ",":
				sc.pos++
			case "]":
				sc.pos++
				return nil
			default:
				return sc.errf("expected , or ] in an array, found %q", sc.peek(1))
			}
		}
	case "{":
		sc.pos++
		for {
			if err := sc.gap(); err != nil {
				return err
			}
			if sc.peek(1) == "}" {
				sc.pos++
				return nil
			}
			if _, err := sc.key(); err != nil {
				return err
			}
			if sc.peek(1) != "=" {
				return sc.errf("expected = in an inline table")
			}
			sc.pos++
			sc.skipSpace()
			if err := sc.value(); err != nil {
				return err
			}
			if err := sc.gap(); err != nil {
				return err
			}
			switch sc.peek(1) {
			case ",":
				sc.pos++
			case "}":
				sc.pos++
				return nil
			default:
				return sc.errf("expected , or } in an inline table, found %q", sc.peek(1))
			}
		}
	}
	start := sc.pos
	for !sc.eof() && !strings.ContainsRune(" \t\r\n,]}#", rune(sc.s[sc.pos])) {
		sc.pos++
	}
	if sc.pos == start {
		return sc.errf("expected a value, found %q", sc.peek(1))
	}
	// A date and a time may be separated by a space.
	if isDate(sc.s[start:sc.pos]) && sc.peek(1) == " " && len(sc.peek(4)) == 4 && isDigit(sc.peek(4)[1]) {
		sc.pos++
		for !sc.eof() && !strings.ContainsRune(" \t\r\n,]}#", rune(sc.s[sc.pos])) {
			sc.pos++
		}
	}
	return nil
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isDate(s string) bool {
	if len(s) != 10 || s[4] != '-' || s[7] != '-' {
		return false
	}
	for _, i := range []int{0, 1, 2, 3, 5, 6, 8, 9} {
		if !isDigit(s[i]) {
			return false
		}
	}
	return true
}

// scanTOML splits a document into its statements.
func scanTOML(content []byte) ([]tomlItem, error) {
	if !utf8.Valid(content) {
		return nil, errors.New("TOML: not UTF-8")
	}
	sc := &tomlScanner{s: string(content)}
	var items []tomlItem
	var table []string
	for !sc.eof() {
		it := tomlItem{start: sc.pos}
		sc.skipSpace()
		switch sc.peek(1) {
		case "", "\n", "\r":
			if err := sc.endLine(); err != nil {
				return nil, err
			}
			it.kind = tomlBlank
		case "#":
			if err := sc.endLine(); err != nil {
				return nil, err
			}
			it.kind = tomlNote
		case "[":
			sc.pos++
			it.kind, it.array = tomlHeader, sc.peek(1) == "["
			if it.array {
				sc.pos++
			}
			path, err := sc.key()
			if err != nil {
				return nil, err
			}
			closing := "]"
			if it.array {
				closing = "]]"
			}
			if sc.peek(len(closing)) != closing {
				return nil, sc.errf("unterminated table header")
			}
			sc.pos += len(closing)
			if err := sc.endLine(); err != nil {
				return nil, err
			}
			it.path, table = path, path
		default:
			key, err := sc.key()
			if err != nil {
				return nil, err
			}
			if sc.peek(1) != "=" {
				return nil, sc.errf("expected = after a key")
			}
			sc.pos++
			sc.skipSpace()
			if err := sc.value(); err != nil {
				return nil, err
			}
			if err := sc.endLine(); err != nil {
				return nil, err
			}
			it.kind, it.path = tomlPair, append(append([]string{}, table...), key...)
		}
		it.end = sc.pos
		items = append(items, it)
	}
	return items, nil
}

// unquoteBasic decodes a basic string, quotes included.
func unquoteBasic(raw string) (string, error) {
	s := raw[1 : len(raw)-1]
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		if i+1 >= len(s) {
			return "", errors.New("bad escape")
		}
		i++
		switch s[i] {
		case 'b':
			b.WriteByte('\b')
		case 't':
			b.WriteByte('\t')
		case 'n':
			b.WriteByte('\n')
		case 'f':
			b.WriteByte('\f')
		case 'r':
			b.WriteByte('\r')
		case 'e':
			b.WriteByte(0x1b)
		case '"':
			b.WriteByte('"')
		case '\\':
			b.WriteByte('\\')
		case 'x', 'u', 'U':
			n := map[byte]int{'x': 2, 'u': 4, 'U': 8}[s[i]]
			if i+1+n > len(s) {
				return "", errors.New("bad escape")
			}
			r, err := strconv.ParseInt(s[i+1:i+1+n], 16, 32)
			if err != nil || !utf8.ValidRune(rune(r)) { //nolint:gosec // 32 bits parsed, so it fits a rune
				return "", errors.New("bad escape")
			}
			b.WriteRune(rune(r)) //nolint:gosec // as above
			i += n
		default:
			return "", fmt.Errorf("bad escape \\%c", s[i])
		}
	}
	return b.String(), nil
}

// tomlString quotes s as a TOML basic string, with only TOML's escapes.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

func harnessLine(harness string) string { return HarnessEnv + " = " + tomlString(harness) }

func tomlTable(e Entry, harness string) []string {
	quoted := make([]string, len(e.Args))
	for i, a := range e.Args {
		quoted[i] = tomlString(a)
	}
	return []string{"[mcp_servers." + ServerName + "]", "command = " + tomlString(e.Command),
		"args = [" + strings.Join(quoted, ", ") + "]", "",
		"[mcp_servers." + ServerName + ".env]", harnessLine(harness)}
}

// starfixPath reports whether a table or key path is inside the starfix
// entry.
func starfixPath(p []string) bool {
	return len(p) >= 2 && p[0] == "mcp_servers" && p[1] == ServerName
}

// tomlSpan is a byte range of the document.
type tomlSpan struct{ start, end int }

// tomlEntry finds the starfix entry's tables: for each, from its header
// to its last key/value pair, with any blank lines after it, so that
// removing the spans leaves no gap. It refuses a starfix entry, or a
// servers table, defined in a form it does not edit.
func tomlEntry(content []byte) ([]tomlSpan, error) {
	items, err := scanTOML(content)
	if err != nil {
		return nil, err
	}
	var spans []tomlSpan
	inEntry, seen := false, false
	for i, it := range items {
		switch it.kind {
		case tomlHeader:
			if len(it.path) == 1 && it.path[0] == "mcp_servers" && it.array {
				return nil, errors.New("mcp_servers is an array of tables; setup edits only [mcp_servers.starfix] tables")
			}
			inEntry = starfixPath(it.path)
			if !inEntry {
				continue
			}
			if len(it.path) == 2 {
				if seen {
					return nil, errors.New("[mcp_servers.starfix] is defined twice")
				}
				seen = true
			}
			end := it.end
			for _, next := range items[i+1:] {
				if next.kind == tomlHeader {
					break
				}
				if next.kind == tomlPair {
					end = next.end
				}
			}
			for _, next := range items[i+1:] {
				if next.start == end && next.kind == tomlBlank {
					end = next.end
				}
			}
			spans = append(spans, tomlSpan{it.start, end})
		case tomlPair:
			if inEntry {
				continue
			}
			if len(it.path) == 1 && it.path[0] == "mcp_servers" {
				return nil, errors.New("mcp_servers is an inline table; make it [mcp_servers.NAME] tables and run setup again")
			}
			if starfixPath(it.path) {
				return nil, errors.New("starfix is defined with dotted keys or an inline table; make it a [mcp_servers.starfix] table and run setup again")
			}
		}
	}
	return spans, nil
}

// cut removes the spans from content and returns the rest, and where the
// first span began (-1 when there was none).
func cut(content []byte, spans []tomlSpan) (rest []byte, at int) {
	at = -1
	prev := 0
	for _, s := range spans {
		if at < 0 {
			at = s.start
		}
		rest = append(rest, content[prev:s.start]...)
		prev = s.end
	}
	return append(rest, content[prev:]...), at
}

func applyTOML(content []byte, e Entry, harness string) ([]byte, error) {
	spans, err := tomlEntry(content)
	if err != nil {
		return nil, err
	}
	block := strings.Join(tomlTable(e, harness), "\n") + "\n"
	rest, at := cut(content, spans)
	if at < 0 {
		// A new entry goes at the end, after a blank line.
		s := string(rest)
		if s != "" && !strings.HasSuffix(s, "\n") {
			s += "\n"
		}
		if t := strings.TrimRight(s, "\r\n \t"); t != "" {
			s = t + "\n\n"
		} else {
			s = ""
		}
		return []byte(s + block), nil
	}
	// The entry replaces the first of its tables, with a blank line
	// before whatever follows; the file ends in one newline.
	after := strings.TrimRight(string(rest[at:]), "\r\n \t")
	if after != "" {
		block += "\n"
		after += "\n"
	}
	return []byte(string(rest[:at]) + block + after), nil
}

// removeTOML takes the starfix entry out, with the blank lines at the
// end of the file.
func removeTOML(content []byte) ([]byte, bool, error) {
	spans, err := tomlEntry(content)
	if err != nil || len(spans) == 0 {
		return content, false, err
	}
	rest, _ := cut(content, spans)
	s := strings.TrimRight(string(rest), "\r\n \t")
	if s == "" {
		return nil, true, nil
	}
	return []byte(s + "\n"), true, nil
}

// tomlRegistered reports whether content holds exactly the entry setup
// writes, in place: applying it again would change nothing.
func tomlRegistered(content []byte, e Entry, harness string) bool {
	out, err := applyTOML(content, e, harness)
	return err == nil && string(out) == string(content)
}

// tomlHas reports whether content has a starfix entry.
func tomlHas(content []byte) bool {
	spans, err := tomlEntry(content)
	return err == nil && len(spans) > 0
}
