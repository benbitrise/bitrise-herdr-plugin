package reconcile

import (
	"errors"
	"testing"

	"github.com/benbitrise/bitrise-herdr-plugin/internal/herdr"
	"github.com/benbitrise/bitrise-herdr-plugin/internal/rde"
	"github.com/benbitrise/bitrise-herdr-plugin/internal/sshconf"
	"github.com/benbitrise/bitrise-herdr-plugin/internal/state"
)

func running(id, name, host string, port int) rde.Session {
	return rde.Session{ID: id, Name: name, Status: rde.StatusRunning, SSHConnectionOpen: true,
		SSHAddress: "ssh vagrant@" + host + " -p " + itoa(port)}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for ; n > 0; n /= 10 {
		s = string(rune('0'+n%10)) + s
	}
	return s
}

var opt = Options{Prefix: "rde-", Herdr: true, IdentityFile: "/k/id_ed25519"}

func threeRunning() []rde.Session {
	return []rde.Session{
		running("aaaaaaaa-1", "Ticket 101", "h1.example", 1001),
		running("bbbbbbbb-2", "ticket 102", "h2.example", 1002),
		running("cccccccc-3", "Simulator (missing manifest)", "h3.example", 1003),
	}
}

func emptyInputs(s []rde.Session) Inputs {
	return Inputs{Sessions: s, SSH: map[string]sshconf.Entry{}, Unmanaged: map[string]bool{}, State: &state.State{Profiles: map[string]state.Profile{}}}
}

func kinds(p Plan) map[Kind][]string {
	m := map[Kind][]string{}
	for _, a := range p.Actions {
		m[a.Kind] = append(m[a.Kind], a.Alias)
	}
	return m
}

// simulate applies a plan to the inputs the way a successful run would.
func simulate(in Inputs, p Plan) Inputs {
	n := 0
	for _, a := range p.Actions {
		switch a.Kind {
		case WriteSSH:
			in.SSH[a.Alias] = a.Entry
		case RemoveSSH:
			delete(in.SSH, a.Alias)
		case AddHerdr:
			n++
			id := "p-" + a.Alias
			in.Machines = append(in.Machines, herdr.Machine{ID: id, Target: a.Alias, Label: a.Label})
			in.State.Profiles[a.Alias] = state.Profile{ID: id, SessionID: a.SessionID}
		case RemoveHerdr:
			var keep []herdr.Machine
			for _, m := range in.Machines {
				if m.ID != a.ProfileID {
					keep = append(keep, m)
				}
			}
			in.Machines = keep
			in.State.Forget(a.ProfileID)
		}
	}
	return in
}

func TestFreshSyncAddsEverySession(t *testing.T) {
	p := Build(emptyInputs(threeRunning()), opt)
	k := kinds(p)
	want := []string{"rde-simulator-missing-manifest", "rde-ticket-101", "rde-ticket-102"}
	if !equal(k[WriteSSH], want) || !equal(k[AddHerdr], want) {
		t.Fatalf("got %v", k)
	}
	if len(k[RemoveSSH])+len(k[RemoveHerdr])+len(k[Skip]) != 0 {
		t.Fatalf("unexpected removals or skips: %v", k)
	}
	for _, a := range p.Actions {
		if a.Kind == WriteSSH && (a.Entry.User != "vagrant" || a.Entry.IdentityFile != "/k/id_ed25519") {
			t.Fatalf("bad entry %+v", a.Entry)
		}
		if a.Kind == AddHerdr && a.Alias == "rde-ticket-101" && a.Label != "Ticket 101" {
			t.Fatalf("label should be the session name, got %q", a.Label)
		}
	}
}

func TestSecondSyncIsANoOp(t *testing.T) {
	in := emptyInputs(threeRunning())
	in = simulate(in, Build(in, opt))
	if p := Build(in, opt); len(p.Actions) != 0 {
		t.Fatalf("second run planned %v", kinds(p))
	}
}

func TestTerminatedSessionIsCleanedUpAlone(t *testing.T) {
	in := emptyInputs(threeRunning())
	in = simulate(in, Build(in, opt))
	in.Sessions[1].Status = rde.StatusTerminated
	p := Build(in, opt)
	k := kinds(p)
	if !equal(k[RemoveHerdr], []string{"rde-ticket-102"}) || !equal(k[RemoveSSH], []string{"rde-ticket-102"}) || len(p.Actions) != 2 {
		t.Fatalf("got %v", k)
	}
	if p.Actions[0].Kind != RemoveHerdr {
		t.Fatalf("Herdr profile must be removed before the SSH entry")
	}
	if p.Actions[1].Entry.HostName != "h2.example" {
		t.Fatalf("RemoveSSH needs the old address for the host key, got %+v", p.Actions[1].Entry)
	}
}

func TestDeletedSessionIsCleanedUp(t *testing.T) {
	in := emptyInputs(threeRunning())
	in = simulate(in, Build(in, opt))
	in.Sessions = in.Sessions[:2]
	k := kinds(Build(in, opt))
	if !equal(k[RemoveHerdr], []string{"rde-simulator-missing-manifest"}) || !equal(k[RemoveSSH], []string{"rde-simulator-missing-manifest"}) {
		t.Fatalf("got %v", k)
	}
}

func TestKeepTerminated(t *testing.T) {
	in := emptyInputs(threeRunning())
	in = simulate(in, Build(in, opt))
	in.Sessions[0].Status = rde.StatusTerminated
	o := opt
	o.KeepTerminated = true
	if p := Build(in, o); len(p.Actions) != 0 {
		t.Fatalf("got %v", kinds(p))
	}
}

func TestTransientStatusesAreLeftAlone(t *testing.T) {
	in := emptyInputs(threeRunning())
	in = simulate(in, Build(in, opt))
	in.Sessions[0].Status = "restoring"
	in.Sessions = append(in.Sessions, rde.Session{ID: "dddddddd-4", Name: "new one", Status: "starting"})
	if p := Build(in, opt); len(p.Actions) != 0 {
		t.Fatalf("got %v", kinds(p))
	}
}

func TestViewFailureNeverRemoves(t *testing.T) {
	in := emptyInputs(threeRunning())
	in = simulate(in, Build(in, opt))
	in.Sessions[0].SSHAddress = ""
	in.ViewErrors = map[string]error{in.Sessions[0].ID: errors.New("500")}
	k := kinds(Build(in, opt))
	if len(k[RemoveSSH])+len(k[RemoveHerdr]) != 0 || !equal(k[Skip], []string{"rde-ticket-101"}) {
		t.Fatalf("got %v", k)
	}
}

func TestSSHNotReadyIsSkipped(t *testing.T) {
	s := threeRunning()[:1]
	s[0].SSHConnectionOpen = false
	k := kinds(Build(emptyInputs(s), opt))
	if !equal(k[Skip], []string{"rde-ticket-101"}) || len(k[WriteSSH]) != 0 {
		t.Fatalf("got %v", k)
	}
}

func TestAddressChangeRewritesEntry(t *testing.T) {
	in := emptyInputs(threeRunning())
	in = simulate(in, Build(in, opt))
	in.Sessions[0].SSHAddress = "ssh vagrant@h9.example -p 2222"
	p := Build(in, opt)
	if len(p.Actions) != 1 || p.Actions[0].Kind != WriteSSH || p.Actions[0].Old == nil || p.Actions[0].Old.HostName != "h1.example" {
		t.Fatalf("got %+v", p.Actions)
	}
}

func TestHandMadeEntriesAreNeverTouched(t *testing.T) {
	// A hand-registered Herdr machine and SSH file, like the original
	// rde-mac1 setup, for a session that no longer exists.
	in := emptyInputs(nil)
	in.Machines = []herdr.Machine{{ID: "hand", Target: "rde-mac1"}, {ID: "other", Target: "devbox"}}
	in.Unmanaged = map[string]bool{"rde-mac1": true}
	if p := Build(in, opt); len(p.Actions) != 0 {
		t.Fatalf("got %v", kinds(p))
	}

	// A running session whose alias collides with a hand-made SSH file is
	// skipped rather than overwritten.
	in = emptyInputs([]rde.Session{running("eeeeeeee-5", "mac1", "h5.example", 1005)})
	in.Unmanaged = map[string]bool{"rde-mac1": true}
	k := kinds(Build(in, opt))
	if !equal(k[Skip], []string{"rde-mac1"}) || len(k[WriteSSH])+len(k[AddHerdr]) != 0 {
		t.Fatalf("got %v", k)
	}
}

func TestExistingHerdrProfileIsReused(t *testing.T) {
	in := emptyInputs(threeRunning()[:1])
	in.Machines = []herdr.Machine{{ID: "hand", Target: "rde-ticket-101"}}
	k := kinds(Build(in, opt))
	if len(k[AddHerdr]) != 0 || !equal(k[WriteSSH], []string{"rde-ticket-101"}) {
		t.Fatalf("got %v", k)
	}
}

func TestSSHOnlyModeIgnoresHerdr(t *testing.T) {
	o := opt
	o.Herdr = false
	in := emptyInputs(threeRunning())
	in.Machines = []herdr.Machine{{ID: "p", Target: "rde-gone"}}
	in.State.Profiles["rde-gone"] = state.Profile{ID: "p"}
	k := kinds(Build(in, o))
	if len(k[AddHerdr])+len(k[RemoveHerdr]) != 0 || len(k[WriteSSH]) != 3 {
		t.Fatalf("got %v", k)
	}
}

func TestAliases(t *testing.T) {
	s := []rde.Session{
		{ID: "11111111-aaaa", Name: "same name"},
		{ID: "22222222-bbbb", Name: "Same Name!"},
		{ID: "33333333-cccc", Name: "🙂"},
		{ID: "44444444-dddd", Name: "a very long session name that goes on and on and on"},
	}
	got := Aliases(s, "rde-", nil)
	want := map[string]string{
		"11111111-aaaa": "rde-same-name-111111",
		"22222222-bbbb": "rde-same-name-222222",
		"33333333-cccc": "rde-333333",
		"44444444-dddd": "rde-a-very-long-session-name-that-goes-on-an",
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s: got %q, want %q", id, got[id], w)
		}
	}
}

// A same-named session appearing or disappearing must not rename a box the
// user is attached to.
func TestAliasesAreSticky(t *testing.T) {
	a := running("aaaaaaaa-1", "foo", "h1.example", 1001)
	b := running("bbbbbbbb-2", "foo", "h2.example", 1002)
	in := emptyInputs([]rde.Session{a})
	in = simulate(in, Build(in, opt))
	if _, ok := in.SSH["rde-foo"]; !ok {
		t.Fatalf("first session: %v", in.SSH)
	}

	in.Sessions = []rde.Session{a, b}
	p := Build(in, opt)
	if p.Aliases[a.ID] != "rde-foo" || p.Aliases[b.ID] != "rde-foo-bbbbbb" {
		t.Fatalf("aliases: %v", p.Aliases)
	}
	k := kinds(p)
	if len(k[RemoveSSH]) > 0 || len(k[RemoveHerdr]) > 0 || !equal(k[WriteSSH], []string{"rde-foo-bbbbbb"}) {
		t.Fatalf("plan: %v", k)
	}
	in = simulate(in, p)

	in.Sessions = []rde.Session{b}
	p = Build(in, opt)
	if p.Aliases[b.ID] != "rde-foo-bbbbbb" || !equal(kinds(p)[RemoveSSH], []string{"rde-foo"}) || len(kinds(p)[WriteSSH]) > 0 {
		t.Fatalf("after the first session is gone: %v %v", p.Aliases, kinds(p))
	}
}

func TestAliasesFollowRenames(t *testing.T) {
	existing := map[string]sshconf.Entry{"rde-old-name": {Alias: "rde-old-name", SessionID: "aaaaaaaa-1"}}
	got := Aliases([]rde.Session{{ID: "aaaaaaaa-1", Name: "new name"}}, "rde-", existing)
	if got["aaaaaaaa-1"] != "rde-new-name" {
		t.Fatalf("got %q", got["aaaaaaaa-1"])
	}
}

func TestNeedsInclude(t *testing.T) {
	in := emptyInputs(threeRunning())
	in = simulate(in, Build(in, opt))
	if p := Build(in, opt); p.Changes() != 0 || !p.NeedsInclude {
		t.Fatalf("an up-to-date plan must still ensure the Include line: %v", kinds(p))
	}
	in.Sessions = nil
	if p := Build(in, opt); p.NeedsInclude {
		t.Fatalf("nothing left to include: %v", kinds(p))
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
