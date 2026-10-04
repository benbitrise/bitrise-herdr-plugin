package main

import (
	"context"
	"io"
	"os"
	"testing"
)

// parseStatus parses args for `status` the way cobra does, without running it.
func parseStatus(args []string) (*config, error) {
	c := &config{}
	root := newRootCmd(context.Background(), c, new(int))
	cmd, _, err := root.Find([]string{"status"})
	if err != nil {
		return nil, err
	}
	if err := cmd.ParseFlags(args); err != nil {
		return nil, err
	}
	return c, root.PersistentPreRunE(cmd, nil)
}

func TestFormatFlags(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "raw"},
		{[]string{"--format", "json"}, "json"},
		{[]string{"-f", "yml"}, "yml"},
		{[]string{"-o", "human"}, "raw"},
		{[]string{"--output=json"}, "json"},
	} {
		c, err := parseStatus(tc.args)
		if err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if c.format != tc.want {
			t.Errorf("%v: format %q, want %q", tc.args, c.format, tc.want)
		}
	}
	if _, err := parseStatus([]string{"-f", "text"}); err == nil {
		t.Error("-f text should be rejected, as the Bitrise CLI does")
	}
	if _, err := parseStatus([]string{"-y"}); err == nil {
		t.Error("-y should be rejected; the Bitrise CLI only has --yes")
	}
}

func TestUsageErrorsExit2(t *testing.T) {
	for _, args := range [][]string{{"attach"}, {"status", "extra"}, {"sync", "--bogus"}, {"nope"}} {
		if code, err := run(context.Background(), args); code != 2 || err == nil {
			t.Errorf("%v: code %d, err %v", args, code, err)
		}
	}
}

func TestWriteYAMLKeepsJSONKeys(t *testing.T) {
	r, w, _ := os.Pipe()
	stdout := os.Stdout
	os.Stdout = w
	(&config{format: "yml"}).writeStructured([]statusRow{{Alias: "rde-a", State: "running", SSH: "ok", Herdr: "ok"}})
	w.Close()
	os.Stdout = stdout
	out, _ := io.ReadAll(r)
	want := "- alias: rde-a\n  state: running\n  ssh: ok\n  herdr: ok\n"
	if string(out) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
}
