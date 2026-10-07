package server

import (
	"errors"
	"testing"
)

func TestRequirePeerCheck(t *testing.T) {
	tests := []struct {
		name         string
		checked, dev bool
		want         error
	}{
		{"linux", true, false, nil},
		{"linux dev", true, true, nil},
		{"other", false, false, ErrNoPeerCheck},
		{"other dev", false, true, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := requirePeerCheck(tt.checked, tt.dev); !errors.Is(got, tt.want) {
				t.Fatalf("requirePeerCheck(%v, %v) = %v, want %v", tt.checked, tt.dev, got, tt.want)
			}
		})
	}
}
