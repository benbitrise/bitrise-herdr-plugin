package reconcile

import (
	"context"
	"errors"
	"testing"

	"github.com/benbitrise/bitrise-herdr-plugin/internal/herdr"
	"github.com/benbitrise/bitrise-herdr-plugin/internal/sshconf"
	"github.com/benbitrise/bitrise-herdr-plugin/internal/state"
)

// A failed Herdr removal keeps the SSH entry, since it's what marks the
// machine as managed on the next run.
func TestFailedHerdrRemovalKeepsSSHEntry(t *testing.T) {
	p := sshconf.DefaultPaths(t.TempDir())
	e := sshconf.Entry{Alias: "rde-x", SessionID: "s1", HostName: "h.example", Port: 22, User: "u"}
	if _, err := p.Write(e); err != nil {
		t.Fatal(err)
	}
	fail := func(context.Context, string, ...string) ([]byte, []byte, error) {
		return nil, []byte("busy"), errors.New("exit 1")
	}
	d := Deps{SSH: p, Prefix: "rde-", Herdr: &herdr.Client{Bin: "herdr", Run: fail}, State: &state.State{Profiles: map[string]state.Profile{}}}
	plan := Plan{Actions: []Action{
		{Kind: RemoveHerdr, Alias: "rde-x", ProfileID: "p1"},
		{Kind: RemoveSSH, Alias: "rde-x", Entry: e},
	}}
	rep, err := Apply(context.Background(), plan, d, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Results[0].Outcome != Failed || rep.Results[1].Outcome != Skipped {
		t.Fatalf("results: %+v", rep.Results)
	}
	if managed, _, _ := p.Scan("rde-"); len(managed) != 1 {
		t.Fatalf("SSH entry was removed: %v", managed)
	}
}
