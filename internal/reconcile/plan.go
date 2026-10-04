// Package reconcile computes and applies the changes that bring SSH entries
// and Herdr profiles in line with the user's RDE sessions. The RDE API is the
// source of truth; everything else is derived state.
package reconcile

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/benbitrise/bitrise-herdr-plugin/internal/herdr"
	"github.com/benbitrise/bitrise-herdr-plugin/internal/rde"
	"github.com/benbitrise/bitrise-herdr-plugin/internal/sshconf"
	"github.com/benbitrise/bitrise-herdr-plugin/internal/state"
)

type Options struct {
	Prefix         string
	KeepTerminated bool
	IdentityFile   string
	Herdr          bool // false for `ssh-config sync`
}

// Inputs are the three state sources. Running sessions in Sessions must
// already carry their SSH details from `session view`.
type Inputs struct {
	Sessions   []rde.Session
	ViewErrors map[string]error // session ID -> failed view
	SSH        map[string]sshconf.Entry
	Unmanaged  map[string]bool // config.d files with the prefix but no marker
	Machines   []herdr.Machine
	State      *state.State
}

type Kind string

const (
	WriteSSH    Kind = "write-ssh"
	RemoveSSH   Kind = "remove-ssh"
	AddHerdr    Kind = "add-herdr"
	RemoveHerdr Kind = "remove-herdr"
	Skip        Kind = "skip"
)

type Action struct {
	Kind        Kind
	Alias       string
	SessionID   string
	SessionName string
	Entry       sshconf.Entry  // WriteSSH: the new entry; RemoveSSH: the old one
	Old         *sshconf.Entry // WriteSSH: the entry being replaced, if any
	ProfileID   string         // RemoveHerdr
	Label       string         // AddHerdr
	Reason      string
}

type Plan struct {
	Actions []Action
	// Aliases maps every listed session ID to its alias.
	Aliases map[string]string
	// NeedsInclude is true when managed SSH entries remain after the plan,
	// so ~/.ssh/config must include them.
	NeedsInclude bool
}

func (p Plan) Changes() int {
	n := 0
	for _, a := range p.Actions {
		if a.Kind != Skip {
			n++
		}
	}
	return n
}

var nonAlias = regexp.MustCompile(`[^a-z0-9]+`)

const maxNameLen = 40

// Sanitize turns a session name into the [a-z0-9-] part of an alias.
func Sanitize(name string) string {
	s := nonAlias.ReplaceAllString(strings.ToLower(name), "-")
	s = strings.Trim(s, "-")
	if len(s) > maxNameLen {
		s = strings.TrimRight(s[:maxNameLen], "-")
	}
	return s
}

// Aliases assigns each session an alias of prefix + sanitized name. Names
// aren't unique, so sessions whose names collide get a short ID suffix. A
// session that already has a managed SSH entry under either form keeps that
// alias, so a same-named session appearing or disappearing never renames a
// box the user is attached to.
func Aliases(sessions []rde.Session, prefix string, existing map[string]sshconf.Entry) map[string]string {
	base := map[string]string{}
	count := map[string]int{}
	for _, s := range sessions {
		b := Sanitize(s.Name)
		if b == "" {
			b = shortID(s.ID)
		}
		base[s.ID] = b
		count[b]++
	}
	suffixed := func(id string) string { return prefix + base[id] + "-" + shortID(id) }

	out := map[string]string{}
	taken := map[string]bool{}
	for _, a := range sshconf.SortedAliases(existing) {
		id := existing[a].SessionID
		if _, listed := base[id]; !listed || out[id] != "" {
			continue
		}
		if a == prefix+base[id] || a == suffixed(id) {
			out[id], taken[a] = a, true
		}
	}
	for _, s := range sessions {
		if out[s.ID] != "" {
			continue
		}
		a := prefix + base[s.ID]
		if count[base[s.ID]] > 1 || taken[a] {
			a = suffixed(s.ID)
		}
		out[s.ID], taken[a] = a, true
	}
	return out
}

func shortID(id string) string {
	id = strings.ReplaceAll(strings.ToLower(id), "-", "")
	if len(id) > 6 {
		return id[:6]
	}
	return id
}

// Build computes the plan. It's pure: no I/O, so running it twice on the same
// inputs gives the same plan, and a run after a successful apply is empty.
func Build(in Inputs, opt Options) Plan {
	aliases := Aliases(in.Sessions, opt.Prefix, in.SSH)
	desired := map[string]sshconf.Entry{}
	names := map[string]string{} // alias -> session name
	keep := map[string]bool{}    // aliases to leave alone
	var acts []Action

	sessions := append([]rde.Session(nil), in.Sessions...)
	sort.Slice(sessions, func(i, j int) bool { return aliases[sessions[i].ID] < aliases[sessions[j].ID] })

	for _, s := range sessions {
		a := aliases[s.ID]
		names[a] = s.Name
		skip := func(reason string) {
			keep[a] = true
			acts = append(acts, Action{Kind: Skip, Alias: a, SessionID: s.ID, SessionName: s.Name, Reason: reason})
		}
		switch s.Status {
		case rde.StatusRunning:
			if err := in.ViewErrors[s.ID]; err != nil {
				skip(fmt.Sprintf("couldn't fetch session details: %v", err))
				continue
			}
			if !s.SSHConnectionOpen || s.SSHAddress == "" {
				skip("SSH isn't ready yet; run sync again in a minute")
				continue
			}
			t, err := rde.ParseSSHAddress(s.SSHAddress)
			if err != nil {
				skip(err.Error())
				continue
			}
			desired[a] = sshconf.Entry{
				Alias: a, SessionID: s.ID, HostName: t.Host, Port: t.Port, User: t.User,
				IdentityFile: opt.IdentityFile,
			}
		case rde.StatusTerminated:
			if opt.KeepTerminated {
				keep[a] = true
			}
		default:
			// Starting, warming up, restoring, terminating and the like:
			// neither add nor remove until the session settles.
			keep[a] = true
		}
	}

	paths := sshconf.Paths{} // Render only needs KnownHosts, which is constant
	for _, a := range sshconf.SortedAliases(desired) {
		e := desired[a]
		if in.Unmanaged[a] {
			acts = append(acts, Action{Kind: Skip, Alias: a, SessionID: e.SessionID, SessionName: names[a],
				Reason: "an SSH config file with this name exists but wasn't created by this plugin; remove it or use another --prefix"})
			delete(desired, a)
			keep[a] = true
			continue
		}
		old, ok := in.SSH[a]
		switch {
		case !ok:
			acts = append(acts, Action{Kind: WriteSSH, Alias: a, SessionID: e.SessionID, SessionName: names[a], Entry: e, Reason: "new session"})
		case paths.Render(old) != paths.Render(e):
			o := old
			acts = append(acts, Action{Kind: WriteSSH, Alias: a, SessionID: e.SessionID, SessionName: names[a], Entry: e, Old: &o, Reason: changeReason(old, e)})
		}
	}

	if opt.Herdr {
		byTarget := map[string][]herdr.Machine{}
		for _, m := range in.Machines {
			byTarget[m.Target] = append(byTarget[m.Target], m)
		}
		// Removals come before SSH removals: profile, then SSH entry, then
		// host key.
		ms := append([]herdr.Machine(nil), in.Machines...)
		sort.Slice(ms, func(i, j int) bool { return ms[i].Target < ms[j].Target })
		for _, m := range ms {
			if !strings.HasPrefix(m.Target, opt.Prefix) {
				continue
			}
			if _, ok := desired[m.Target]; ok || keep[m.Target] {
				continue
			}
			_, hasManagedSSH := in.SSH[m.Target]
			if !in.State.Owns(m.ID) && !hasManagedSSH {
				continue // hand-registered; never touch
			}
			acts = append(acts, Action{Kind: RemoveHerdr, Alias: m.Target, ProfileID: m.ID,
				SessionID: in.SSH[m.Target].SessionID, SessionName: names[m.Target], Reason: goneReason(m.Target, names)})
		}
		for _, a := range sshconf.SortedAliases(desired) {
			if len(byTarget[a]) > 0 {
				continue
			}
			e := desired[a]
			acts = append(acts, Action{Kind: AddHerdr, Alias: a, SessionID: e.SessionID, SessionName: names[a], Label: names[a], Reason: "no Herdr profile"})
		}
	}

	needsInclude := len(desired) > 0
	for _, a := range sshconf.SortedAliases(in.SSH) {
		if _, ok := desired[a]; ok || keep[a] {
			needsInclude = true
			continue
		}
		old := in.SSH[a]
		acts = append(acts, Action{Kind: RemoveSSH, Alias: a, SessionID: old.SessionID, SessionName: names[a], Entry: old, Reason: goneReason(a, names)})
	}

	sort.SliceStable(acts, func(i, j int) bool { return order(acts[i].Kind) < order(acts[j].Kind) })
	return Plan{Actions: acts, Aliases: aliases, NeedsInclude: needsInclude}
}

func order(k Kind) int {
	switch k {
	case RemoveHerdr:
		return 0
	case RemoveSSH:
		return 1
	case WriteSSH:
		return 2
	case AddHerdr:
		return 3
	}
	return 4
}

func changeReason(old, e sshconf.Entry) string {
	if old.HostName != e.HostName || old.Port != e.Port {
		return fmt.Sprintf("address changed from %s:%d to %s:%d", old.HostName, old.Port, e.HostName, e.Port)
	}
	return "settings changed"
}

func goneReason(alias string, names map[string]string) string {
	if _, listed := names[alias]; listed {
		return "session is terminated"
	}
	return "session no longer exists"
}
