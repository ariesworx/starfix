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
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("not JSON: %w", err)
		}
		key, _ := tok.(string)
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

func (o object) get(key string) (json.RawMessage, bool) {
	for _, m := range o {
		if m.key == key {
			return m.val, true
		}
	}
	return nil, false
}

// set replaces key's value in place, or appends it.
func (o object) set(key string, v json.RawMessage) object {
	for i := range o {
		if o[i].key == key {
			o[i].val = v
			return o
		}
	}
	return append(o, member{key, v})
}

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

func indent(raw json.RawMessage) ([]byte, error) {
	var b bytes.Buffer
	if err := json.Indent(&b, raw, "", "  "); err != nil {
		return nil, fmt.Errorf("format JSON: %w", err)
	}
	b.WriteByte('\n')
	return b.Bytes(), nil
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("agentsetup: %v", err)) // strings and string slices always marshal
	}
	return b
}

// owned are the entry keys starfix writes, in order; other keys are left.
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
	entry := object{}
	if raw, ok := servers.get(ServerName); ok {
		if entry, err = parseObject(raw); err != nil {
			return nil, fmt.Errorf("%s.%s: %w", key, ServerName, err)
		}
	}
	for _, m := range owned(f, e) {
		entry = entry.set(m.key, m.val)
	}
	env := object{}
	if raw, ok := entry.get("env"); ok {
		if env, err = parseObject(raw); err != nil {
			return nil, fmt.Errorf("%s.%s.env: %w", key, ServerName, err)
		}
	}
	entry = entry.set("env", env.set(HarnessEnv, mustJSON(harness)).marshal())
	servers = servers.set(ServerName, entry.marshal())
	root = root.set(key, servers.marshal())
	return indent(root.marshal())
}

func removeJSON(content []byte, f format) ([]byte, error) {
	root, err := parseObject(content)
	if err != nil {
		return nil, err
	}
	key := serversKey(f)
	raw, _ := root.get(key)
	servers, err := parseObject(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", key, err)
	}
	root = root.set(key, servers.del(ServerName).marshal())
	return indent(root.marshal())
}

// jsonEntry returns the starfix entry, if content has one.
func jsonEntry(content []byte, f format) (object, bool) {
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
	raw, ok = servers.get(ServerName)
	if !ok {
		return nil, false
	}
	entry, err := parseObject(raw)
	return entry, err == nil
}

func jsonRegistered(content []byte, f format, e Entry, harness string) bool {
	entry, ok := jsonEntry(content, f)
	if !ok {
		return false
	}
	for _, m := range owned(f, e) {
		got, ok := entry.get(m.key)
		if !ok || !sameJSON(got, m.val) {
			return false
		}
	}
	raw, _ := entry.get("env")
	env, err := parseObject(raw)
	if err != nil || len(raw) == 0 {
		return false
	}
	got, ok := env.get(HarnessEnv)
	return ok && sameJSON(got, mustJSON(harness))
}

func sameJSON(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}
