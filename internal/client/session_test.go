package client

import "testing"

func TestSessionFromEnv(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"none", nil, ""},
		{"claude code", map[string]string{"CLAUDE_CODE_SESSION_ID": "cc-1"}, "cc-1"},
		{"older claude spelling", map[string]string{"CLAUDE_SESSION_ID": "c-1"}, "c-1"},
		{"claude code over older", map[string]string{"CLAUDE_SESSION_ID": "c-1", "CLAUDE_CODE_SESSION_ID": "cc-1"}, "cc-1"},
		{"starfix wins", map[string]string{"STARFIX_SESSION": "sf-1", "CLAUDE_CODE_SESSION_ID": "cc-1"}, "sf-1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := SessionFromEnv(func(k string) string { return tc.env[k] }); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
