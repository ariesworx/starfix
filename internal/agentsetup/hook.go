package agentsetup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// The SessionStart hook runs `sfx prime --hook=AGENT` when a session
// starts, so the agent begins oriented. Claude Code's form, which Codex
// and Junie share, nests the hook in a group:
//
//	{"hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "sfx prime --hook"}]}]}}
//
// Gemini CLI uses the same nesting but matches a group's matcher to the
// session's source exactly, so it gets one group per source. Cursor and
// VS Code put the hook straight in the event's list (hookStyle.flat).
//
// A hook is starfix's when its command runs prime --hook, with or without
// =AGENT, through sfx or through the program setup was given; other hooks
// are left alone.

// hookStyle is how one harness writes its SessionStart hook.
type hookStyle struct {
	// event is the key under "hooks" that lists SessionStart hooks.
	event string
	// flat puts hooks straight in the event's list, not in groups.
	flat bool
	// matchers are the groups' matchers, one group each; "" writes a
	// group without one, and a flat style has the one "". With a single
	// matcher, starfix's hook may sit in any group: the person may have
	// moved it.
	matchers []string
	// untyped hooks have no "type": "command".
	untyped bool
	// timeout is written in a new hook, in the harness's unit; 0 for none.
	timeout int
	// version is written as the file's "version" when it has none.
	version int
	// own: the file is starfix's alone, deleted when Remove empties it.
	own bool
}

var (
	// claudeHook is Claude Code's, Junie's, and with a matcher Codex's.
	claudeHook = hookStyle{event: "SessionStart", matchers: []string{""}}
	// codexHook: Codex's matcher is a regex on the session's source.
	codexHook = hookStyle{event: "SessionStart", matchers: []string{"startup|resume|clear|compact"}}
	// geminiHook: Gemini's matcher is an exact source, and its timeout is
	// in milliseconds.
	geminiHook = hookStyle{event: "SessionStart", matchers: []string{"startup", "resume", "clear"}, timeout: 15000}
	cursorHook = hookStyle{event: "sessionStart", flat: true, matchers: []string{""}, untyped: true, timeout: 30, version: 1}
	vscodeHook = hookStyle{event: "SessionStart", flat: true, matchers: []string{""}, timeout: 15, own: true}
)

var (
	shellSafe = regexp.MustCompile(`^[A-Za-z0-9_./:@%+=,-]+$`)
	// hookCmd splits a prime hook's command into its program and harness.
	hookCmd = regexp.MustCompile(`^(.+) prime --hook(?:=[a-z][a-z-]*)?$`)
)

// shellQuote quotes s for the POSIX shell the harness runs hooks with.
func shellQuote(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// HookCommand is the command the harness's hook runs. Claude Code's is
// the bare --hook, as before other harnesses had one.
func HookCommand(e Entry, harness string) string {
	if harness == "claude-code" {
		return shellQuote(e.Command) + " prime --hook"
	}
	return shellQuote(e.Command) + " prime --hook=" + harness
}

// HookOutput is the hook's stdout document, carrying context for the
// session: Cursor's own shape, else Claude Code's, which the others share.
func (a Agent) HookOutput(context string) any {
	if a.hook.event == "sessionStart" {
		return map[string]string{"additional_context": context}
	}
	return map[string]any{"hookSpecificOutput": map[string]string{
		"hookEventName": "SessionStart", "additionalContext": context}}
}

// Hooks lists the agents setup writes a hook for, sorted: the values
// `sfx prime --hook=` takes.
func Hooks() []string {
	var out []string
	for _, n := range Names() {
		if a := Agents[n]; a.Hook != "" || a.GlobalHook != "" {
			out = append(out, n)
		}
	}
	return out
}

// program is a command's program name, without directory, quotes or .exe.
func program(cmd string) string {
	cmd = strings.Trim(cmd, `'"`)
	if i := strings.LastIndexAny(cmd, `/\`); i >= 0 {
		cmd = cmd[i+1:]
	}
	return strings.TrimSuffix(strings.ToLower(cmd), ".exe")
}

// hookParts reads a hook's type and command; ok is false when it is not
// a command hook.
func hookParts(h object) (cmd string, ok bool) {
	if raw, has := h.get("type"); has {
		var typ string
		if json.Unmarshal(raw, &typ) != nil || typ != "command" {
			return "", false
		}
	}
	raw, has := h.get("command")
	if !has || json.Unmarshal(raw, &cmd) != nil {
		return "", false
	}
	return cmd, true
}

func ourHook(h object, e Entry, harness string) bool {
	cmd, ok := hookParts(h)
	if !ok {
		return false
	}
	if cmd == HookCommand(e, harness) {
		return true
	}
	m := hookCmd.FindStringSubmatch(cmd)
	return m != nil && (program(m[1]) == "sfx" || program(m[1]) == program(e.Command))
}

// exactHook reports whether h runs exactly the command setup writes.
func exactHook(h object, e Entry, harness string) bool {
	cmd, ok := hookParts(h)
	return ok && cmd == HookCommand(e, harness)
}

func parseArray(raw json.RawMessage) ([]json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if t := bytes.TrimSpace(raw); len(t) == 0 || t[0] != '[' {
		return nil, fmt.Errorf("not a JSON array")
	}
	var a []json.RawMessage
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("not JSON: %w", err)
	}
	return a, nil
}

func marshalArray(a []json.RawMessage) json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('[')
	for i, v := range a {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(v)
	}
	b.WriteByte(']')
	return b.Bytes()
}

// hookDoc is a settings file opened down to its SessionStart list: the
// groups, or for a flat style the hooks themselves.
type hookDoc struct {
	style       hookStyle
	root, hooks object
	list        []json.RawMessage
}

func (s hookStyle) open(content []byte) (*hookDoc, error) {
	d := &hookDoc{style: s, hooks: object{}}
	var err error
	if d.root, err = parseObject(content); err != nil {
		return nil, err
	}
	if raw, ok := d.root.get("hooks"); ok {
		if d.hooks, err = parseObject(raw); err != nil {
			return nil, fmt.Errorf("hooks: %w", err)
		}
	}
	raw, _ := d.hooks.get(s.event)
	if d.list, err = parseArray(raw); err != nil {
		return nil, fmt.Errorf("hooks.%s: %w", s.event, err)
	}
	return d, nil
}

// edit calls f on each group's matcher and hook list and keeps what it
// returns; a group f empties is dropped. A flat list is one group with
// no matcher.
func (d *hookDoc) edit(f func(matcher string, hs []json.RawMessage) ([]json.RawMessage, error)) error {
	path := "hooks." + d.style.event
	if d.style.flat {
		kept, err := f("", d.list)
		d.list = kept
		return err
	}
	var out []json.RawMessage
	for i, raw := range d.list {
		g, err := parseObject(raw)
		if err != nil {
			return fmt.Errorf("%s[%d]: %w", path, i, err)
		}
		hraw, _ := g.get("hooks")
		hs, err := parseArray(hraw)
		if err != nil {
			return fmt.Errorf("%s[%d].hooks: %w", path, i, err)
		}
		var matcher string
		if m, ok := g.get("matcher"); ok {
			_ = json.Unmarshal(m, &matcher) // not a string: matches no source of ours
		}
		kept, err := f(matcher, hs)
		if err != nil {
			return err
		}
		switch {
		case len(kept) == 0 && len(hs) > 0:
			continue
		case string(marshalArray(kept)) == string(marshalArray(hs)):
			out = append(out, raw)
		default:
			out = append(out, g.set("hooks", marshalArray(kept)).marshal())
		}
	}
	d.list = out
	return nil
}

// slot is the index in style.matchers that a group with this matcher
// fills, or -1.
func (s hookStyle) slot(matcher string) int {
	if len(s.matchers) == 1 {
		return 0
	}
	return slices.Index(s.matchers, matcher)
}

func (d *hookDoc) marshal() ([]byte, error) {
	if len(d.list) > 0 {
		d.hooks = d.hooks.set(d.style.event, marshalArray(d.list))
	} else {
		d.hooks = d.hooks.del(d.style.event)
	}
	if len(d.hooks) > 0 {
		d.root = d.root.set("hooks", d.hooks.marshal())
	} else {
		d.root = d.root.del("hooks")
	}
	if d.style.own && len(d.root) == 0 {
		return nil, nil
	}
	return indent(d.root.marshal())
}

func (s hookStyle) entry(e Entry, harness string) json.RawMessage {
	o := object{}
	if !s.untyped {
		o = o.set("type", mustJSON("command"))
	}
	o = o.set("command", mustJSON(HookCommand(e, harness)))
	if s.timeout > 0 {
		o = o.set("timeout", mustJSON(s.timeout))
	}
	return o.marshal()
}

func (s hookStyle) group(matcher string, e Entry, harness string) json.RawMessage {
	g := object{}
	if matcher != "" {
		g = g.set("matcher", mustJSON(matcher))
	}
	return g.set("hooks", marshalArray([]json.RawMessage{s.entry(e, harness)})).marshal()
}

// apply puts the hook in place: it updates an existing starfix hook's
// command, keeping its other keys, and adds a hook, or a group holding
// one, for each matcher that has none. A duplicate is dropped.
func (s hookStyle) apply(content []byte, e Entry, harness string) ([]byte, Result, error) {
	if s.registered(content, e, harness) {
		return content, Unchanged, nil
	}
	d, err := s.open(content)
	if err != nil {
		return nil, Unchanged, err
	}
	filled := make([]bool, len(s.matchers))
	found := false
	err = d.edit(func(matcher string, hs []json.RawMessage) ([]json.RawMessage, error) {
		out := make([]json.RawMessage, 0, len(hs))
		for _, raw := range hs {
			h, err := parseObject(raw)
			if err != nil || !ourHook(h, e, harness) {
				out = append(out, raw)
				continue
			}
			found = true
			if i := s.slot(matcher); i >= 0 && !filled[i] {
				filled[i] = true
				out = append(out, h.set("command", mustJSON(HookCommand(e, harness))).marshal())
			}
		}
		return out, nil
	})
	if err != nil {
		return nil, Unchanged, err
	}
	for i, m := range s.matchers {
		switch {
		case filled[i]:
		case s.flat:
			d.list = append(d.list, s.entry(e, harness))
		default:
			d.list = append(d.list, s.group(m, e, harness))
		}
	}
	if _, ok := d.root.get("version"); s.version > 0 && !ok {
		d.root = append(object{{"version", mustJSON(s.version)}}, d.root...)
	}
	res := Added
	if found {
		res = Updated
	}
	out, err := d.marshal()
	return out, res, err
}

func (s hookStyle) remove(content []byte, e Entry, harness string) ([]byte, Result, error) {
	d, err := s.open(content)
	if err != nil {
		return nil, Unchanged, err
	}
	found := false
	err = d.edit(func(_ string, hs []json.RawMessage) ([]json.RawMessage, error) {
		out := make([]json.RawMessage, 0, len(hs))
		for _, raw := range hs {
			if h, err := parseObject(raw); err == nil && ourHook(h, e, harness) {
				found = true
				continue
			}
			out = append(out, raw)
		}
		return out, nil
	})
	if err != nil || !found {
		return content, Unchanged, err
	}
	out, err := d.marshal()
	return out, Removed, err
}

// registered reports whether every matcher's group has a hook running
// exactly HookCommand(e, harness).
func (s hookStyle) registered(content []byte, e Entry, harness string) bool {
	d, err := s.open(content)
	if err != nil {
		return false
	}
	filled := make([]bool, len(s.matchers))
	_ = d.edit(func(matcher string, hs []json.RawMessage) ([]json.RawMessage, error) {
		for _, raw := range hs {
			if h, err := parseObject(raw); err == nil && exactHook(h, e, harness) {
				if i := s.slot(matcher); i >= 0 {
					filled[i] = true
				}
			}
		}
		return hs, nil
	}) // f never fails; a malformed group just is not ours
	return !slices.Contains(filled, false)
}

func (s hookStyle) snippet(e Entry, harness string) string {
	b, _, err := s.apply(nil, e, harness)
	if err != nil {
		panic(fmt.Sprintf("agentsetup: hook snippet: %v", err)) // only on a programming error
	}
	return string(b)
}
