package agentsetup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// The SessionStart hook runs `sfx prime --hook` when a Claude Code session
// starts, resumes, clears or compacts, so the agent begins oriented:
//
//	{"hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "sfx prime --hook"}]}]}}
//
// A hook is starfix's when its command runs prime --hook through sfx, or
// through the program setup was given; other hooks are left alone.

const hookArgs = " prime --hook"

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_./:@%+=,-]+$`)

// shellQuote quotes s for the POSIX shell the harness runs hooks with.
func shellQuote(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// HookCommand is the command the hook runs.
func HookCommand(e Entry) string { return shellQuote(e.Command) + hookArgs }

// program is a command's program name, without directory, quotes or .exe.
func program(cmd string) string {
	cmd = strings.Trim(cmd, `'"`)
	if i := strings.LastIndexAny(cmd, `/\`); i >= 0 {
		cmd = cmd[i+1:]
	}
	return strings.TrimSuffix(strings.ToLower(cmd), ".exe")
}

func ourHook(h object, e Entry) bool {
	var typ, cmd string
	if raw, ok := h.get("type"); !ok || json.Unmarshal(raw, &typ) != nil || typ != "command" {
		return false
	}
	if raw, ok := h.get("command"); !ok || json.Unmarshal(raw, &cmd) != nil {
		return false
	}
	if cmd == HookCommand(e) {
		return true
	}
	prog, ok := strings.CutSuffix(cmd, hookArgs)
	return ok && (program(prog) == "sfx" || program(prog) == program(e.Command))
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

// hookDoc is a settings file opened down to its SessionStart groups.
type hookDoc struct {
	root, hooks object
	groups      []json.RawMessage
}

func openHooks(content []byte) (*hookDoc, error) {
	d := &hookDoc{hooks: object{}}
	var err error
	if d.root, err = parseObject(content); err != nil {
		return nil, err
	}
	if raw, ok := d.root.get("hooks"); ok {
		if d.hooks, err = parseObject(raw); err != nil {
			return nil, fmt.Errorf("hooks: %w", err)
		}
	}
	raw, _ := d.hooks.get("SessionStart")
	if d.groups, err = parseArray(raw); err != nil {
		return nil, fmt.Errorf("hooks.SessionStart: %w", err)
	}
	return d, nil
}

// edit calls f on each group's hook list and keeps what it returns; a
// group f empties is dropped.
func (d *hookDoc) edit(f func([]json.RawMessage) ([]json.RawMessage, error)) error {
	var out []json.RawMessage
	for i, raw := range d.groups {
		g, err := parseObject(raw)
		if err != nil {
			return fmt.Errorf("hooks.SessionStart[%d]: %w", i, err)
		}
		hraw, _ := g.get("hooks")
		hs, err := parseArray(hraw)
		if err != nil {
			return fmt.Errorf("hooks.SessionStart[%d].hooks: %w", i, err)
		}
		kept, err := f(hs)
		if err != nil {
			return err
		}
		switch {
		case len(kept) == 0 && len(hs) > 0:
			continue
		case len(kept) == len(hs) && string(marshalArray(kept)) == string(marshalArray(hs)):
			out = append(out, raw)
		default:
			out = append(out, g.set("hooks", marshalArray(kept)).marshal())
		}
	}
	d.groups = out
	return nil
}

func (d *hookDoc) marshal() ([]byte, error) {
	if len(d.groups) > 0 {
		d.hooks = d.hooks.set("SessionStart", marshalArray(d.groups))
	} else {
		d.hooks = d.hooks.del("SessionStart")
	}
	if len(d.hooks) > 0 {
		d.root = d.root.set("hooks", d.hooks.marshal())
	} else {
		d.root = d.root.del("hooks")
	}
	return indent(d.root.marshal())
}

func hookEntry(e Entry) json.RawMessage {
	return object{{"type", mustJSON("command")}, {"command", mustJSON(HookCommand(e))}}.marshal()
}

// applyHook puts the hook in place: it updates an existing starfix hook's
// command, or adds a group holding the hook.
func applyHook(content []byte, e Entry) ([]byte, Result, error) {
	if hookRegistered(content, e) {
		return content, Unchanged, nil
	}
	d, err := openHooks(content)
	if err != nil {
		return nil, Unchanged, err
	}
	found := false
	err = d.edit(func(hs []json.RawMessage) ([]json.RawMessage, error) {
		out := make([]json.RawMessage, 0, len(hs))
		for _, raw := range hs {
			h, err := parseObject(raw)
			if err != nil || !ourHook(h, e) {
				out = append(out, raw)
				continue
			}
			if !found {
				found = true
				out = append(out, h.set("command", mustJSON(HookCommand(e))).marshal())
			} // a duplicate is dropped
		}
		return out, nil
	})
	if err != nil {
		return nil, Unchanged, err
	}
	res := Updated
	if !found {
		res = Added
		d.groups = append(d.groups, object{{"hooks", marshalArray([]json.RawMessage{hookEntry(e)})}}.marshal())
	}
	out, err := d.marshal()
	return out, res, err
}

func removeHook(content []byte, e Entry) ([]byte, Result, error) {
	d, err := openHooks(content)
	if err != nil {
		return nil, Unchanged, err
	}
	found := false
	err = d.edit(func(hs []json.RawMessage) ([]json.RawMessage, error) {
		out := make([]json.RawMessage, 0, len(hs))
		for _, raw := range hs {
			if h, err := parseObject(raw); err == nil && ourHook(h, e) {
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

// hookRegistered reports whether a SessionStart hook runs exactly
// HookCommand(e).
func hookRegistered(content []byte, e Entry) bool {
	d, err := openHooks(content)
	if err != nil {
		return false
	}
	found := false
	_ = d.edit(func(hs []json.RawMessage) ([]json.RawMessage, error) {
		for _, raw := range hs {
			h, err := parseObject(raw)
			if err != nil {
				continue
			}
			var typ, cmd string
			t, _ := h.get("type")
			c, _ := h.get("command")
			if json.Unmarshal(t, &typ) == nil && json.Unmarshal(c, &cmd) == nil && typ == "command" && cmd == HookCommand(e) {
				found = true
			}
		}
		return hs, nil
	}) // f never fails; a malformed group just is not ours
	return found
}

func hookSnippet(e Entry) string {
	b, err := indent(object{{"hooks", object{{"SessionStart",
		marshalArray([]json.RawMessage{object{{"hooks", marshalArray([]json.RawMessage{hookEntry(e)})}}.marshal()})}}.marshal()}}.marshal())
	if err != nil {
		panic(fmt.Sprintf("agentsetup: hook snippet: %v", err)) // only on a programming error
	}
	return string(b)
}
