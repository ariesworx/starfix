package agentsetup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
)

// member is one key of a JSON object, its value kept as written.
type member struct {
	key string
	val json.RawMessage
}

// object is a JSON object that keeps its key order.
type object []member

// parseObject reads a JSON object, keeping its key order and each value
// as written. Empty input is an empty object, and a duplicate key is an
// error.
func parseObject(b []byte) (object, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return object{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("not JSON: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("not a JSON object")
	}
	var o object
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("not JSON: %w", err)
		}
		key, _ := tok.(string)
		if seen[key] {
			// Harnesses read JSON as JavaScript's JSON.parse does, which
			// keeps the last copy: editing the first would leave the
			// harness running the other.
			return nil, fmt.Errorf("duplicate key %q; keep one and run setup again", key)
		}
		seen[key] = true
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, fmt.Errorf("not JSON: %w", err)
		}
		o = append(o, member{key, v})
	}
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("not JSON: %w", err)
	}
	if _, err := dec.Token(); err == nil {
		return nil, errors.New("not JSON: text after the object")
	}
	return o, nil
}

// get returns key's value and whether o has key.
func (o object) get(key string) (json.RawMessage, bool) {
	for _, m := range o {
		if m.key == key {
			return m.val, true
		}
	}
	return nil, false
}

// set replaces key's value in place, or appends it. It changes o's
// array, so use the result in place of o.
func (o object) set(key string, v json.RawMessage) object {
	for i := range o {
		if o[i].key == key {
			o[i].val = v
			return o
		}
	}
	return append(o, member{key, v})
}

// del removes key. It reuses o's array, so use the result in place of o.
func (o object) del(key string) object {
	out := o[:0]
	for _, m := range o {
		if m.key != key {
			out = append(out, m)
		}
	}
	return out
}

// marshal writes o compactly; indent formats the whole document.
func (o object) marshal() json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, m := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(m.key)
		b.Write(k)
		b.WriteByte(':')
		b.Write(m.val)
	}
	b.WriteByte('}')
	return b.Bytes()
}

// indent formats a document as setup writes JSON files: indented two
// spaces, with a final newline.
func indent(raw json.RawMessage) ([]byte, error) {
	var b bytes.Buffer
	if err := json.Indent(&b, raw, "", "  "); err != nil {
		return nil, fmt.Errorf("format JSON: %w", err)
	}
	b.WriteByte('\n')
	return b.Bytes(), nil
}

// mustJSON marshals a value that cannot fail to: a string, a string
// slice or an int.
func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("agentsetup: %v", err)) // only on a programming error
	}
	return b
}

// owned are the entry keys starfix writes, in order, env aside.
func owned(f format, e Entry) object {
	o := object{}
	if f == jsonClaude || f == jsonVSCode {
		o = o.set("type", mustJSON("stdio"))
	}
	o = o.set("command", mustJSON(e.Command))
	return o.set("args", mustJSON(e.Args))
}

// serversKey is the top-level key that holds the servers.
func serversKey(f format) string {
	if f == jsonVSCode {
		return "servers"
	}
	return "mcpServers"
}

// applyJSON registers e under its server name, replacing any entry of
// that name in its place among the servers, and returns the formatted
// document.
func applyJSON(content []byte, f format, e Entry, harness string) ([]byte, error) {
	root, err := parseObject(content)
	if err != nil {
		return nil, err
	}
	key := serversKey(f)
	servers := object{}
	if raw, ok := root.get(key); ok {
		if servers, err = parseObject(raw); err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
	}
	// The entry is replaced wholesale, in its place among the servers.
	entry := owned(f, e).set("env", object{}.set(HarnessEnv, mustJSON(harness)).marshal())
	servers = servers.set(e.server(), entry.marshal())
	root = root.set(key, servers.marshal())
	return indent(root.marshal())
}

// removeJSON takes out the named server; found is false when there is
// none.
func removeJSON(content []byte, f format, name string) (out []byte, found bool, err error) {
	root, err := parseObject(content)
	if err != nil {
		return nil, false, err
	}
	key := serversKey(f)
	raw, _ := root.get(key)
	servers, err := parseObject(raw)
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w", key, err)
	}
	if _, ok := servers.get(name); !ok {
		return content, false, nil
	}
	root = root.set(key, servers.del(name).marshal())
	out, err = indent(root.marshal())
	return out, err == nil, err
}

// jsonEntry returns the named server's entry, if content has one.
func jsonEntry(content []byte, f format, name string) (object, bool) {
	root, err := parseObject(content)
	if err != nil {
		return nil, false
	}
	raw, ok := root.get(serversKey(f))
	if !ok {
		return nil, false
	}
	servers, err := parseObject(raw)
	if err != nil {
		return nil, false
	}
	raw, ok = servers.get(name)
	if !ok {
		return nil, false
	}
	entry, err := parseObject(raw)
	return entry, err == nil
}

// jsonRegistered reports whether the entry is exactly the one setup
// writes, in any key order: no key, and no env variable, besides.
func jsonRegistered(content []byte, f format, e Entry, harness string) bool {
	root, err := parseObject(content)
	if err != nil {
		return false
	}
	raw, _ := root.get(serversKey(f))
	servers, err := parseObject(raw)
	if err != nil {
		return false
	}
	raw, ok := servers.get(e.server())
	if !ok {
		return false
	}
	entry, err := parseObject(raw)
	want := owned(f, e)
	if err != nil || len(entry) != len(want)+1 {
		return false
	}
	for _, m := range want {
		got, ok := entry.get(m.key)
		if !ok || !sameJSON(got, m.val) {
			return false
		}
	}
	raw, _ = entry.get("env")
	env, err := parseObject(raw)
	if err != nil || len(raw) == 0 || len(env) != 1 {
		return false
	}
	got, ok := env.get(HarnessEnv)
	return ok && sameJSON(got, mustJSON(harness))
}

// sameJSON reports whether a and b decode to the same value.
func sameJSON(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}
