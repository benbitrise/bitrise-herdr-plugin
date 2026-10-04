package rde

import (
	"context"
	"testing"
)

func TestParseSSHAddress(t *testing.T) {
	for addr, want := range map[string]Target{
		"ssh vagrant@vm-standalone-macos-b67c.5.remote-access.bitrise.io -p 28985": {"vagrant", "vm-standalone-macos-b67c.5.remote-access.bitrise.io", 28985},
		"ssh -p 2222 ubuntu@linux.example":                                         {"ubuntu", "linux.example", 2222},
		"ubuntu@linux.example":                                                     {"ubuntu", "linux.example", 22},
	} {
		got, err := ParseSSHAddress(addr)
		if err != nil || got != want {
			t.Errorf("%q: got %+v, %v", addr, got, err)
		}
	}
	for _, bad := range []string{"", "host.example:22", "ssh host.example"} {
		if _, err := ParseSSHAddress(bad); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

func TestListRejectsUnexpectedShape(t *testing.T) {
	c := &Client{Bin: "bitrise", Run: func(context.Context, string, ...string) ([]byte, []byte, error) {
		return []byte(`{"sessions": []}`), nil, nil
	}}
	if _, err := c.List(context.Background()); err == nil {
		t.Fatal("a missing items field must be an error, not an empty list")
	}
}
