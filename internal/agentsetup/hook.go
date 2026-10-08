package agentsetup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// The SessionStart hook runs `sfx prime --hook=AGENT` (bare --hook for
// Claude Code) when a session starts, so the agent begins oriented.
// Claude Code also gets usage hooks: `sfx usage --hook` on Stop,
// SubagentStop and SessionEnd sends the turn's token counts from its
// transcript. Stop and SubagentStop run async, so a slow server never
// holds up the agent; SessionEnd, which sends the session's last
// response, runs synchronously, since Claude Code waits for it as it
// exits, up to the hook's timeout (at most 60 s), and an async hook may
// not outlive the exit.
// Claude Code's form, which Codex and Junie share, nests the hook in a
// group:
//
//	{"hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "sfx prime --hook"}]}]}}
//
// Gemini CLI uses the same nesting but matches a group's matcher to the
// session's source exactly, so it gets one group per source. Cursor and
// VS Code put the hook straight in the event's list (hookStyle.flat).
//
// A hook is starfix's when its whole command is one that setup writes:
// a single program word, bare or single-quoted as shellQuote writes it,
// naming sfx or the program setup was given, then the hook's verb (prime
// or usage) and --hook, with or without =AGENT. Any other hook, a
// person's compound command that ends in `sfx prime --hook` included, is
// left alone.

// hookStyle is how one harness writes one of starfix's hooks.
type hookStyle struct {
	// event is the key under "hooks" that lists the event's hooks.
	event string
	// verb is the sfx command the hook runs with --hook: prime or usage.
	verb string
	// flat puts hooks straight in the event's list, not in groups.
	flat bool
	// matchers are the groups' matchers, one group each; "" writes a
	// group without one, and a flat style has the one "". With a single
	// matcher, starfix's hook may sit in any group: the person may have
	// moved it.
	matchers []string
	// untyped hooks have no "type": "command".
	untyped bool
	// async runs the hook in the background (Claude Code's "async").
	async bool
	// timeout is written in a new hook, in the harness's unit; 0 for none.
	timeout int
	// version is written as the file's "version" when it has none.
	version int
	// own: the file is starfix's alone, deleted when Remove empties it.
	own bool
}

var (
	// claudeHook is Claude Code's, Junie's, and with a matcher Codex's.
	claudeHook = hookStyle{event: "SessionStart", verb: "prime", matchers: []string{""}}
	// codexHook: Codex's matcher is a regex on the session's source.
	codexHook = hookStyle{event: "SessionStart", verb: "prime", matchers: []string{"startup|resume|clear|compact"}}
	// geminiHook: Gemini's matcher is an exact source, and its timeout is
	// in milliseconds.
	geminiHook = hookStyle{event: "SessionStart", verb: "prime", matchers: []string{"startup", "resume", "clear"}, timeout: 15000}
	// cursorHook: Cursor's hooks are flat and untyped, under
	// sessionStart, in a file with a version.
	cursorHook = hookStyle{event: "sessionStart", verb: "prime", flat: true, matchers: []string{""}, untyped: true, timeout: 30, version: 1}
	// vscodeHook: VS Code's hooks are flat, in a file of starfix's own
	// (.github/hooks/starfix.json).
	vscodeHook = hookStyle{event: "SessionStart", verb: "prime", flat: true, matchers: []string{""}, timeout: 15, own: true}
)

// UsageTimeout is the timeout setup gives Claude Code's async usage hooks
// (Stop and SubagentStop), and SessionEndTimeout its synchronous
// SessionEnd hook, which Claude Code waits for as it exits. `sfx usage
// --hook` gives up before each.
const (
	UsageTimeout      = 30 * time.Second
	SessionEndTimeout = 10 * time.Second
)

// claudeUsage is Claude Code's usage hook on event: async, in the
// background, so the turn does not wait on the server, or synchronous
// with a shorter timeout. Timeouts are in seconds. A group without a
// matcher runs for every subagent type and session end reason.
func claudeUsage(event string, async bool, timeout time.Duration) hookStyle {
	return hookStyle{event: event, verb: "usage", matchers: []string{""}, async: async, timeout: int(timeout / time.Second)}
}

// claudeHooks are Claude Code's: prime at session start, and usage at
// the end of each turn, each subagent and the session.
var claudeHooks = []hookStyle{claudeHook, claudeUsage("Stop", true, UsageTimeout),
	claudeUsage("SubagentStop", true, UsageTimeout), claudeUsage("SessionEnd", false, SessionEndTimeout)}

var (
	// shellSafe matches a word the shell reads as itself, which
	// shellQuote leaves bare.
	shellSafe = regexp.MustCompile(`^[A-Za-z0-9_./:@%+=,-]+$`)
	// hookCmd splits a hook's command into its program, one word as
	// shellQuote writes it, and its verb.
	hookCmd = regexp.MustCompile(`^([A-Za-z0-9_./:@%+=,-]+|'(?:[^']|'\\'')*') (prime|usage) --hook(?:=[a-z][a-z-]*)?$`)
)

// shellQuote quotes s for the POSIX shell the harness runs hooks with.
func shellQuote(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// HookCommand is the command the harness's SessionStart hook runs: e's
// program, quoted for the shell, then prime --hook=harness, or bare
// --hook for Claude Code, which sfx prime reads as claude-code.
func HookCommand(e Entry, harness string) string {
	return hookStyle{verb: "prime"}.command(e, harness)
}

// command is the command the hook runs: e's program, quoted for the
// shell, then the style's verb and --hook=harness, or bare --hook for
// Claude Code.
func (s hookStyle) command(e Entry, harness string) string {
	cmd := shellQuote(e.Command) + " " + s.verb + " --hook"
	if harness != "claude-code" {
		cmd += "=" + harness
	}
	return cmd
}

// HookOutput is the SessionStart hook's stdout document, carrying
// context for the session: Cursor's own shape, else Claude Code's, which
// the others share.
func (a Agent) HookOutput(context string) any {
	if len(a.hooks) > 0 && a.hooks[0].event == "sessionStart" {
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

// UsageHooks lists the agents setup writes usage hooks for, sorted: the
// values `sfx usage --hook=` takes.
func UsageHooks() []string {
	var out []string
	for _, n := range Names() {
		if slices.ContainsFunc(Agents[n].hooks, func(s hookStyle) bool { return s.verb == "usage" }) {
			out = append(out, n)
		}
	}
	return out
}

// program is a command's program name, without directory, quotes or .exe.
func program(cmd string) string {
	if len(cmd) >= 2 && cmd[0] == '\'' {
		cmd = strings.ReplaceAll(cmd[1:len(cmd)-1], `'\''`, "'")
	}
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

// ours reports whether h is this style's starfix hook: it runs exactly
// s.command(e, harness), or one program word as shellQuote writes it,
// naming sfx or e's program, then s.verb --hook with or without =AGENT.
func (s hookStyle) ours(h object, e Entry, harness string) bool {
	cmd, ok := hookParts(h)
	if !ok {
		return false
	}
	if cmd == s.command(e, harness) {
		return true
	}
	m := hookCmd.FindStringSubmatch(cmd)
	return m != nil && m[2] == s.verb && (program(m[1]) == "sfx" || program(m[1]) == program(e.Command))
}

// exact reports whether h runs exactly the command setup writes, async
// or not as setup writes it. Its timeout is the person's to change.
func (s hookStyle) exact(h object, e Entry, harness string) bool {
	cmd, ok := hookParts(h)
	return ok && cmd == s.command(e, harness) && async(h) == s.async
}

// async reports whether h is set to run in the background.
func async(h object) bool {
	raw, ok := h.get("async")
	var b bool
	return ok && json.Unmarshal(raw, &b) == nil && b
}

// fix is h, one of this style's hooks, with the command setup writes. A
// hook async where the style is not, or the reverse, also gets the
// style's async setting and timeout, which go together; its other keys
// are kept.
func (s hookStyle) fix(h object, e Entry, harness string) object {
	h = h.set("command", mustJSON(s.command(e, harness)))
	if async(h) == s.async {
		return h
	}
	if s.async {
		h = h.set("async", mustJSON(true))
	} else {
		h = h.del("async")
	}
	if s.timeout > 0 {
		h = h.set("timeout", mustJSON(s.timeout))
	}
	return h
}

// parseArray reads a JSON array, keeping each element as written; empty
// input is an empty array.
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

// marshalArray writes a compactly, each element as it was.
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

// hookDoc is a settings file opened down to one event's list: the
// groups, or for a flat style the hooks themselves.
type hookDoc struct {
	style       hookStyle
	root, hooks object
	list        []json.RawMessage
}

// open reads content down to the style's event list. A missing level is
// empty; a level of the wrong type is an error naming its path.
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

// marshal writes the list back into the document, dropping the levels
// it leaves empty, and formats it. A file of starfix's own that is left
// empty comes back nil, to be deleted.
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

// entry is a new hook for e, in the style's shape.
func (s hookStyle) entry(e Entry, harness string) json.RawMessage {
	o := object{}
	if !s.untyped {
		o = o.set("type", mustJSON("command"))
	}
	o = o.set("command", mustJSON(s.command(e, harness)))
	if s.async {
		o = o.set("async", mustJSON(true))
	}
	if s.timeout > 0 {
		o = o.set("timeout", mustJSON(s.timeout))
	}
	return o.marshal()
}

// group is a new group holding a new hook, with matcher if it is set.
func (s hookStyle) group(matcher string, e Entry, harness string) json.RawMessage {
	g := object{}
	if matcher != "" {
		g = g.set("matcher", mustJSON(matcher))
	}
	return g.set("hooks", marshalArray([]json.RawMessage{s.entry(e, harness)})).marshal()
}

// apply puts the hook in place: it updates an existing starfix hook's
// command and async setting (fix), keeping its other keys, and adds a hook, or a group holding
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
			if err != nil || !s.ours(h, e, harness) {
				out = append(out, raw)
				continue
			}
			found = true
			if i := s.slot(matcher); i >= 0 && !filled[i] {
				filled[i] = true
				out = append(out, s.fix(h, e, harness).marshal())
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

// remove takes out every starfix hook, keeping the person's hooks and
// dropping the groups it empties.
func (s hookStyle) remove(content []byte, e Entry, harness string) ([]byte, Result, error) {
	d, err := s.open(content)
	if err != nil {
		return nil, Unchanged, err
	}
	found := false
	err = d.edit(func(_ string, hs []json.RawMessage) ([]json.RawMessage, error) {
		out := make([]json.RawMessage, 0, len(hs))
		for _, raw := range hs {
			if h, err := parseObject(raw); err == nil && s.ours(h, e, harness) {
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
// exactly s.command(e, harness).
func (s hookStyle) registered(content []byte, e Entry, harness string) bool {
	d, err := s.open(content)
	if err != nil {
		return false
	}
	filled := make([]bool, len(s.matchers))
	_ = d.edit(func(matcher string, hs []json.RawMessage) ([]json.RawMessage, error) {
		for _, raw := range hs {
			if h, err := parseObject(raw); err == nil && s.exact(h, e, harness) {
				if i := s.slot(matcher); i >= 0 {
					filled[i] = true
				}
			}
		}
		return hs, nil
	}) // f never fails; a malformed group just is not ours
	return !slices.Contains(filled, false)
}

// The functions below apply an agent's hook styles, each its own event
// in one settings file, as one target.

// applyHooks puts every style's hook in place. The result is Unchanged
// when none changed, Added when each was added, and Updated otherwise,
// as when an install from before the usage hooks gains them.
func applyHooks(styles []hookStyle, content []byte, e Entry, harness string) ([]byte, Result, error) {
	out, changed, added := content, false, true
	for _, s := range styles {
		var res Result
		var err error
		if out, res, err = s.apply(out, e, harness); err != nil {
			return nil, Unchanged, err
		}
		changed = changed || res != Unchanged
		added = added && res == Added
	}
	switch {
	case !changed:
		return content, Unchanged, nil
	case added:
		return out, Added, nil
	}
	return out, Updated, nil
}

// removeHooks takes every style's starfix hooks out; empty output means
// the file was starfix's own and may be deleted.
func removeHooks(styles []hookStyle, content []byte, e Entry, harness string) ([]byte, Result, error) {
	out, removed := content, false
	for _, s := range styles {
		var res Result
		var err error
		if out, res, err = s.remove(out, e, harness); err != nil {
			return nil, Unchanged, err
		}
		removed = removed || res == Removed
	}
	if !removed {
		return content, Unchanged, nil
	}
	return out, Removed, nil
}

// missingHooks lists the events whose hooks are not registered as setup
// writes them, in the styles' order.
func missingHooks(styles []hookStyle, content []byte, e Entry, harness string) []string {
	var out []string
	for _, s := range styles {
		if !s.registered(content, e, harness) {
			out = append(out, s.event)
		}
	}
	return out
}

// hooksSnippet is every style's hook, as a new file holding them, to
// paste by hand.
func hooksSnippet(styles []hookStyle, e Entry, harness string) string {
	b, _, err := applyHooks(styles, nil, e, harness)
	if err != nil {
		panic(fmt.Sprintf("agentsetup: hook snippet: %v", err)) // only on a programming error
	}
	return string(b)
}
