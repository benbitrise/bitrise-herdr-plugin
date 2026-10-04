package reconcile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/benbitrise/bitrise-herdr-plugin/internal/herdr"
	"github.com/benbitrise/bitrise-herdr-plugin/internal/remote"
	"github.com/benbitrise/bitrise-herdr-plugin/internal/sshconf"
	"github.com/benbitrise/bitrise-herdr-plugin/internal/state"
)

type Deps struct {
	SSH      sshconf.Paths
	Prefix   string
	Herdr    *herdr.Client
	Probe    *remote.Prober
	Local    herdr.Status // laptop's herdr
	State    *state.State
	Log      io.Writer // progress; keep stdout clean for --format json
	Parallel int

	// CheckUntil, if set, retries a failing readiness check every CheckPoll
	// until then, for a box whose startup script is still installing things.
	CheckUntil time.Time
	CheckPoll  time.Duration
}

type Outcome string

const (
	Done    Outcome = "done"
	Planned Outcome = "planned"
	Skipped Outcome = "skipped"
	Failed  Outcome = "failed"
)

type Result struct {
	Alias     string  `json:"alias"`
	Session   string  `json:"session,omitempty"`
	SessionID string  `json:"session_id,omitempty"`
	Action    Kind    `json:"action"`
	Outcome   Outcome `json:"outcome"`
	Detail    string  `json:"detail,omitempty"`
}

type Report struct {
	DryRun         bool     `json:"dry_run"`
	IncludeChanged bool     `json:"include_changed"`
	Results        []Result `json:"results"`
}

func (r Report) Failed() bool {
	for _, x := range r.Results {
		if x.Outcome == Failed {
			return true
		}
	}
	return false
}

func result(a Action, o Outcome, detail string) Result {
	if detail == "" {
		detail = a.Reason
	}
	return Result{Alias: a.Alias, Session: a.SessionName, SessionID: a.SessionID, Action: a.Kind, Outcome: o, Detail: detail}
}

// Apply carries out a plan in order: Herdr removals, SSH removals, SSH
// writes, then Herdr adds. A failed step for one session never stops the
// others.
func Apply(ctx context.Context, plan Plan, d Deps, dryRun bool) (Report, error) {
	rep := Report{DryRun: dryRun}
	logf := func(format string, args ...any) {
		if d.Log != nil {
			fmt.Fprintf(d.Log, format+"\n", args...)
		}
	}

	if plan.NeedsInclude {
		changed, err := d.SSH.EnsureInclude(d.Prefix, dryRun)
		if err != nil {
			return rep, fmt.Errorf("update %s: %w", d.SSH.ConfigFile, err)
		}
		rep.IncludeChanged = changed
	}

	removedSSH := map[string]bool{}
	failedWrite := map[string]bool{}
	failedRemove := map[string]bool{} // Herdr removals that failed
	var adds []Action
	for _, a := range plan.Actions {
		if dryRun {
			o := Planned
			if a.Kind == Skip {
				o = Skipped
			}
			detail := a.Reason
			if a.Kind == AddHerdr {
				detail += "; SSH and Herdr compatibility are checked before adding"
			}
			rep.Results = append(rep.Results, result(a, o, detail))
			continue
		}
		switch a.Kind {
		case Skip:
			rep.Results = append(rep.Results, result(a, Skipped, ""))

		case RemoveHerdr:
			logf("Removing Herdr machine %s (%s)…", a.Alias, a.ProfileID)
			if err := d.Herdr.Remove(ctx, a.ProfileID); err != nil {
				failedRemove[a.Alias] = true
				rep.Results = append(rep.Results, result(a, Failed, err.Error()))
				continue
			}
			d.State.Forget(a.ProfileID)
			rep.Results = append(rep.Results, result(a, Done, ""))

		case RemoveSSH:
			// The managed SSH entry is what marks the machine as ours when
			// the state file doesn't, so keep it until the machine is gone.
			if failedRemove[a.Alias] {
				rep.Results = append(rep.Results, result(a, Skipped, "kept because the Herdr machine wasn't removed; run the command again to retry"))
				continue
			}
			logf("Removing SSH entry %s…", a.Alias)
			if err := d.SSH.Remove(a.Alias); err != nil {
				rep.Results = append(rep.Results, result(a, Failed, err.Error()))
				continue
			}
			removedSSH[a.Alias] = true
			detail := a.Reason
			if err := d.SSH.RemoveHostKey(ctx, a.Entry.HostName, a.Entry.Port); err != nil {
				detail += "; host key not removed: " + err.Error()
			}
			rep.Results = append(rep.Results, result(a, Done, detail))

		case WriteSSH:
			logf("Writing SSH entry %s…", a.Alias)
			// Drop any key recorded for this address: RDE reuses hostnames
			// and ports, and a stale key would fail the connection.
			_ = d.SSH.RemoveHostKey(ctx, a.Entry.HostName, a.Entry.Port)
			if a.Old != nil && (a.Old.HostName != a.Entry.HostName || a.Old.Port != a.Entry.Port) {
				_ = d.SSH.RemoveHostKey(ctx, a.Old.HostName, a.Old.Port)
			}
			if _, err := d.SSH.Write(a.Entry); err != nil {
				failedWrite[a.Alias] = true
				rep.Results = append(rep.Results, result(a, Failed, err.Error()))
				continue
			}
			rep.Results = append(rep.Results, result(a, Done, ""))

		case AddHerdr:
			if failedWrite[a.Alias] {
				rep.Results = append(rep.Results, result(a, Skipped, "SSH entry couldn't be written"))
				continue
			}
			adds = append(adds, a)
		}
	}

	if len(adds) > 0 {
		rep.Results = append(rep.Results, addMachines(ctx, adds, plan, d, logf)...)
	}
	return rep, nil
}

// addMachines checks every box in parallel, then runs herdr machine add one
// at a time, because it may prompt on the terminal.
func addMachines(ctx context.Context, adds []Action, plan Plan, d Deps, logf func(string, ...any)) []Result {
	type checked struct {
		ok       bool
		res      Result
		warnings []string
	}
	out := make([]checked, len(adds))
	sem := make(chan struct{}, max(1, d.Parallel))
	var wg sync.WaitGroup
	for i, a := range adds {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			logf("Checking %s over SSH…", a.Alias)
			warnings, err := checkUntilReady(ctx, d, entryFor(a, plan, d), logf)
			if err != nil {
				out[i] = checked{res: result(a, Skipped, err.Error())}
				return
			}
			out[i] = checked{ok: true, warnings: warnings}
		}()
	}
	wg.Wait()

	var results []Result
	for i, a := range adds {
		c := out[i]
		if !c.ok {
			results = append(results, c.res)
			continue
		}
		logf("Adding Herdr machine %s…", a.Alias)
		if err := d.Herdr.Add(ctx, a.Alias, a.Label); err != nil {
			results = append(results, result(a, Failed, err.Error()))
			continue
		}
		detail := a.Reason
		if id, err := findProfile(ctx, d.Herdr, a.Alias); err != nil {
			detail = "added, but couldn't read its profile ID, so a later sync won't remove it: " + err.Error()
		} else {
			d.State.Profiles[a.Alias] = state.Profile{ID: id, SessionID: a.SessionID, AddedAt: time.Now().UTC()}
		}
		if len(c.warnings) > 0 {
			detail += "; warning: " + strings.Join(c.warnings, "; ")
		}
		results = append(results, result(a, Done, detail))
	}
	return results
}

func checkUntilReady(ctx context.Context, d Deps, e sshconf.Entry, logf func(string, ...any)) ([]string, error) {
	last := ""
	for {
		warnings, err := Check(ctx, d, e)
		if err == nil || !time.Now().Before(d.CheckUntil) {
			return warnings, err
		}
		if err.Error() != last {
			logf("Waiting for %s to be ready: %v", e.Alias, err)
			last = err.Error()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(d.CheckPoll):
		}
	}
}

// entryFor finds the SSH entry an add depends on: the one written in this
// run, or the existing managed one.
func entryFor(a Action, plan Plan, d Deps) sshconf.Entry {
	for _, x := range plan.Actions {
		if x.Kind == WriteSSH && x.Alias == a.Alias {
			return x.Entry
		}
	}
	managed, _, _ := d.SSH.Scan(d.Prefix)
	return managed[a.Alias]
}

func findProfile(ctx context.Context, c *herdr.Client, target string) (string, error) {
	ms, err := c.List(ctx)
	if err != nil {
		return "", err
	}
	for _, m := range ms {
		if m.Target == target {
			return m.ID, nil
		}
	}
	return "", errors.New("no machine with that target after add")
}

// Check confirms a box is ready for Herdr: the alias resolves, SSH works
// without a prompt, and the box's herdr is one machine add accepts as is. It
// returns non-fatal warnings, or an error naming the fix.
func Check(ctx context.Context, d Deps, e sshconf.Entry) ([]string, error) {
	if e.Alias == "" {
		return nil, errors.New("no managed SSH entry for this session")
	}
	if err := d.Probe.CheckConfig(ctx, e.Alias, e.HostName, e.Port); err != nil {
		return nil, err
	}
	err := d.Probe.Reach(ctx, e.Alias)
	var f *remote.Failure
	if errors.As(err, &f) && f.Kind == remote.HostKey {
		// A reused hostname with a new key: drop the stale one and retry.
		if rmErr := d.SSH.RemoveHostKey(ctx, e.HostName, e.Port); rmErr == nil {
			err = d.Probe.Reach(ctx, e.Alias)
		}
	}
	if err != nil {
		return nil, explain(err, e)
	}

	h, err := d.Probe.Health(ctx, e.Alias)
	if err != nil {
		return nil, explain(err, e)
	}
	if h.LoginHerdr == "" {
		return nil, fmt.Errorf("herdr isn't installed on the box. Template fix: install herdr %s in the warmup script and run `herdr integration install claude`", d.Local.Version)
	}
	if why := herdr.Incompatibility(d.Local, h.Herdr); why != "" {
		boxVersion := ""
		if h.Herdr != nil {
			boxVersion = h.Herdr.Version
		}
		return nil, fmt.Errorf("the box's herdr (%s) %s, so machine add would prompt to replace it. Template fix: install herdr %s in the warmup script (or update the laptop)", orUnknown(boxVersion), why, d.Local.Version)
	}
	var warnings []string
	if h.LoginClaude == "" {
		warnings = append(warnings, "claude isn't on the box's login PATH, so agents won't start until the template installs Claude Code onto PATH")
	}
	if h.NonInteractiveHerdr == "" || h.NonInteractiveClaude == "" {
		warnings = append(warnings, "herdr/claude aren't on the non-interactive SSH PATH; template fix: export PATH in ~/.zshenv (macOS) or ~/.profile")
	}
	return warnings, nil
}

func explain(err error, e sshconf.Entry) error {
	var f *remote.Failure
	if !errors.As(err, &f) {
		return err
	}
	switch f.Kind {
	case remote.Auth:
		key := e.IdentityFile
		if key == "" {
			key = "your SSH key"
		}
		return fmt.Errorf("the box rejected the key (%s). Check that the template's SSH_PUBLIC_KEY saved input matches %s.pub, and run `ssh-add` if the key has a passphrase", f.Detail, key)
	case remote.Network:
		return fmt.Errorf("couldn't reach %s:%d (%s); the session may still be starting, or SSH isn't open", e.HostName, e.Port, f.Detail)
	case remote.HostKey:
		return fmt.Errorf("host key check failed even after removing the old key from the RDE known_hosts file (%s)", f.Detail)
	}
	return err
}

func orUnknown(s string) string {
	if s == "" {
		return "an unknown version"
	}
	return s
}
