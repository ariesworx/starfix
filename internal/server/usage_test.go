package server

import (
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

func i64(v int64) *int64 { return &v }

// usageRec is a request record at at with input in and output 0.
func usageRec(id string, at time.Time, in int64) proto.UsageRecord {
	return proto.UsageRecord{Harness: "claude-code", RequestID: id, Model: "opus", At: at, Granularity: "request",
		Tokens: proto.Tokens{Input: i64(in), Output: i64(0)}}
}

// A protocol 2 server, as v0.2.x runs, refuses a protocol 3 client at the
// handshake, telling it to have the server upgraded, before any request
// could fail on an op or field it does not know.
func TestHandshakeProtocol2ServerRefusesNewClient(t *testing.T) {
	s := newServer(t)
	s.cfg.ProtoMin, s.cfg.ProtoMax = 2, 2
	f, err := handshake(t, s, &proto.Frame{T: proto.FrameBridge, Principal: "alice"},
		&proto.Frame{T: proto.FrameHello, Version: "v0.3.0", Proto: proto.Proto, Project: project, Session: "s", Machine: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if f.Err == nil || f.Err.Code != proto.CodeVersion || !strings.Contains(f.Err.Message, "accepts 2 to 2") ||
		!strings.Contains(f.Err.Fix, "starfixd upgrade") {
		t.Fatalf("protocol %d client to a protocol 2 server: %+v, want a version refusal naming starfixd upgrade", proto.Proto, f.Err)
	}
}

func TestDispatchUsage(t *testing.T) {
	s := newServer(t)
	now := time.Now().UTC()
	batch := proto.UsageArgs{Records: []proto.UsageRecord{usageRec("r1", now, 10), usageRec("r2", now, 20)}}
	if got := mustCall[proto.UsageResult](t, s, alice, proto.OpUsage, batch); got != (proto.UsageResult{Added: 2}) {
		t.Errorf("usage = %+v, want 2 added", got)
	}
	if got := mustCall[proto.UsageResult](t, s, alice, proto.OpUsage, batch); got != (proto.UsageResult{Duplicates: 2}) {
		t.Errorf("usage again = %+v, want 2 duplicates", got)
	}

	tests := []struct {
		name string
		args string
		code proto.Code
		msg  string
		fix  string
	}{
		{"invalid record", `{"records":[{"harness":"x","request_id":"r","model":"m","at":"` + now.Format(time.RFC3339) +
			`","granularity":"hourly"}]}`, proto.CodeInvalid, "usage record 1: granularity", "nothing from the batch was stored"},
		{"negative count", `{"records":[{"harness":"x","request_id":"r","model":"m","at":"` + now.Format(time.RFC3339) +
			`","granularity":"request","input":-5}]}`, proto.CodeInvalid, "usage record 1: input", "nothing from the batch was stored"},
		// Identity comes from the connection, never from the arguments.
		{"principal in a record", `{"records":[{"harness":"x","request_id":"r","model":"m","at":"` + now.Format(time.RFC3339) +
			`","granularity":"request","principal":"bob"}]}`, proto.CodeInvalid, "unknown field", proto.FixUpgrade},
		{"cost in a record", `{"records":[{"harness":"x","request_id":"r","model":"m","at":"` + now.Format(time.RFC3339) +
			`","granularity":"request","cost_usd":1.5}]}`, proto.CodeInvalid, "unknown field", proto.FixUpgrade},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, perr := s.Dispatch(t.Context(), alice, proto.OpUsage, json.RawMessage(tc.args))
			if perr == nil || perr.Code != tc.code || !strings.Contains(perr.Message, tc.msg) || !strings.Contains(perr.Fix, tc.fix) {
				t.Fatalf("usage(%s) = %+v; want %s naming %q, fix with %q", tc.name, perr, tc.code, tc.msg, tc.fix)
			}
		})
	}
}

func TestDispatchUsageLimits(t *testing.T) {
	s := newServerWith(t, Limits{Limits: store.Limits{UsageRecords: 2, UsagePerDay: 3}})
	now := time.Now().UTC()
	recs := func(ids ...string) proto.UsageArgs {
		var a proto.UsageArgs
		for _, id := range ids {
			a.Records = append(a.Records, usageRec(id, now, 1))
		}
		return a
	}
	_, perr := call[proto.UsageResult](t, s, alice, proto.OpUsage, recs("a", "b", "c"))
	if perr == nil || perr.Code != proto.CodeInvalid || !strings.Contains(perr.Message, "at most 2 usage records") ||
		!strings.Contains(perr.Fix, "send at most 2") {
		t.Fatalf("3 records with usage_records 2: %+v, want invalid with a fix to send fewer", perr)
	}
	mustCall[proto.UsageResult](t, s, alice, proto.OpUsage, recs("a", "b"))
	_, perr = call[proto.UsageResult](t, s, alice, proto.OpUsage, recs("c", "d"))
	if perr == nil || perr.Code != proto.CodeBusy || !strings.Contains(perr.Message, "3 usage records a day") ||
		!strings.Contains(perr.Fix, "usage_per_day") {
		t.Fatalf("past usage_per_day: %+v, want busy naming the limit", perr)
	}
}

func TestDispatchShowUsageAndAccount(t *testing.T) {
	s := newServer(t)
	epic := mustCall[proto.CreateResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "epic", Type: "epic", Account: "acme"})
	task := mustCall[proto.CreateResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "task", Parent: epic.ID})
	// A protocol 2 client sends no account; the issue inherits the default.
	raw, perr := s.Dispatch(t.Context(), alice, proto.OpCreate, json.RawMessage(`{"title":"from an old client"}`))
	if perr != nil {
		t.Fatalf("create without account: %v", perr)
	}
	orphan := raw.(proto.CreateResult)

	mustCall[proto.StartResult](t, s, alice, proto.OpStart, proto.StartArgs{ID: task.ID})
	at := time.Now().UTC()
	mustCall[proto.UsageResult](t, s, alice, proto.OpUsage, proto.UsageArgs{Records: []proto.UsageRecord{usageRec("r1", at, 42)}})

	tests := []struct {
		name, id, account, from string
		models                  string
	}{
		{"own account", epic.ID, "acme", epic.ID, ""},
		{"inherited, with tokens", task.ID, "acme", epic.ID, "opus 42/0"},
		{"server default", orphan.ID, "internal", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := mustCall[proto.ShowResult](t, s, alice, proto.OpShow, proto.ShowArgs{ID: tc.id})
			u := r.Usage
			if u == nil {
				t.Fatal("show has no usage")
			}
			var models []string
			for _, m := range u.Models {
				models = append(models, m.Model+" "+itoa(m.Input)+"/"+itoa(m.Output))
			}
			if u.Account != tc.account || u.AccountFrom != tc.from || strings.Join(models, "; ") != tc.models {
				t.Errorf("show(%s).usage = account %q from %q, models %q; want %q from %q, %q",
					tc.id, u.Account, u.AccountFrom, models, tc.account, tc.from, tc.models)
			}
		})
	}

	// Update sets and clears the account, and refuses a non-code name.
	cur := mustCall[proto.ShowResult](t, s, alice, proto.OpShow, proto.ShowArgs{ID: orphan.ID})
	w := mustCall[proto.WriteResult](t, s, alice, proto.OpUpdate, proto.UpdateArgs{ID: orphan.ID, Rev: cur.Issue.Rev, Account: ptr("ops")})
	if r := mustCall[proto.ShowResult](t, s, alice, proto.OpShow, proto.ShowArgs{ID: orphan.ID}); r.Issue.Account != "ops" || r.Usage.Account != "ops" {
		t.Errorf("after update: issue account %q, usage account %q; want ops", r.Issue.Account, r.Usage.Account)
	}
	_, perr = call[proto.WriteResult](t, s, alice, proto.OpUpdate, proto.UpdateArgs{ID: orphan.ID, Rev: w.Rev, Account: ptr("Acme Corp")})
	if perr == nil || perr.Code != proto.CodeInvalid || !strings.Contains(perr.Message, "code name") {
		t.Errorf("update to account \"Acme Corp\" = %+v, want invalid naming a code name", perr)
	}
	mustCall[proto.WriteResult](t, s, alice, proto.OpUpdate, proto.UpdateArgs{ID: orphan.ID, Rev: w.Rev, Account: ptr("")})
	if r := mustCall[proto.ShowResult](t, s, alice, proto.OpShow, proto.ShowArgs{ID: orphan.ID}); r.Issue.Account != "" || r.Usage.Account != "internal" {
		t.Errorf("after clearing: issue account %q, usage account %q; want none, inheriting internal", r.Issue.Account, r.Usage.Account)
	}

	d := mustCall[proto.DigestResult](t, s, alice, proto.OpDigest, proto.DigestArgs{})
	if d.Usage == nil || len(d.Usage.Models) != 1 || itoa(d.Usage.Models[0].Input) != "42" {
		t.Errorf("digest usage = %+v, want opus with input 42", d.Usage)
	}
}

func TestServerAccountDefault(t *testing.T) {
	s := newServer(t)
	st := s.cfg.Store
	for _, tc := range []struct {
		account string
		ok      bool
	}{{"", true}, {"ops-team", true}, {"Ops Team", false}} {
		_, err := New(Config{Store: st, Project: project, Account: tc.account, PeerCheck: func(net.Conn) error { return nil }})
		if (err == nil) != tc.ok {
			t.Errorf("New(Account %q) = %v, want ok %v", tc.account, err, tc.ok)
		}
	}
}

func itoa(p *int64) string {
	if p == nil {
		return "?"
	}
	b, _ := json.Marshal(*p)
	return string(b)
}

func ptr[T any](v T) *T { return &v }
