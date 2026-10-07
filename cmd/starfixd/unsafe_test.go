package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
)

// S-2, decision D3: the admin commands refuse an unsafe Dolt account, as
// serve does; --allow-unsafe-dolt lets one through only with --dev.
func TestUnsafeDoltAccount(t *testing.T) {
	cfg, err := mysql.ParseDSN(newDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	root := dolt.RootDSN(cfg.DBName)
	tests := []struct {
		name  string
		args  []string
		usage bool
		err   string
	}{
		{name: "root refused", err: "dolt account root@localhost is unsafe"},
		{name: "escape needs --dev", args: []string{"--allow-unsafe-dolt"}, usage: true, err: "needs --dev"},
		{name: "--dev alone refuses still", args: []string{"--dev"}, err: "is unsafe"},
		{name: "escape with --dev", args: []string{"--dev", "--allow-unsafe-dolt"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := admin(t, root, "", exportBD, tc.args...)
			var ue usageError
			switch {
			case tc.err == "" && r.err != nil:
				t.Fatalf("export-bd %v: %v", tc.args, r.err)
			case tc.err != "" && (r.err == nil || !strings.Contains(r.err.Error(), tc.err)):
				t.Fatalf("export-bd %v = %v, want an error naming %q", tc.args, r.err, tc.err)
			case tc.usage != errors.As(r.err, &ue):
				t.Errorf("export-bd %v = %v; usage error %v, want %v", tc.args, r.err, !tc.usage, tc.usage)
			}
			if tc.err != "" && !tc.usage && strings.Count(r.err.Error(), "fix: ") != 1 {
				t.Errorf("refusal %q has %d fixes, want one", r.err, strings.Count(r.err.Error(), "fix: "))
			}
		})
	}
}

func TestServeUnsafeNeedsDev(t *testing.T) {
	err := serve(t.Context(), []string{"--allow-unsafe-dolt"})
	var ue usageError
	if !errors.As(err, &ue) || !strings.Contains(err.Error(), "needs --dev") {
		t.Errorf("serve --allow-unsafe-dolt = %v, want a usage error naming --dev", err)
	}
}
