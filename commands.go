package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/benbitrise/bitrise-herdr-plugin/internal/herdr"
	"github.com/benbitrise/bitrise-herdr-plugin/internal/rde"
	"github.com/benbitrise/bitrise-herdr-plugin/internal/reconcile"
	"github.com/benbitrise/bitrise-herdr-plugin/internal/remote"
	"github.com/benbitrise/bitrise-herdr-plugin/internal/sshconf"
	"github.com/benbitrise/bitrise-herdr-plugin/internal/state"
	"gopkg.in/yaml.v3"
)

type config struct {
	workspace      string
	prefix         string
	format         string
	dryRun         bool
	yes            bool
	identityFile   string
	parallel       int
	keepTerminated bool
	label          string
	wait           bool
	waitTimeout    time.Duration
	waitUntil      time.Time // attach --wait deadline, shared by every stage of the wait
	failOnSkip     bool

	home      string
	ssh       sshconf.Paths
	statePath string
	rde       *rde.Client
	herdr     *herdr.Client
	probe     *remote.Prober
}

// validate checks the parsed flags and sets up paths and clients.
func (c *config) validate() error {
	switch c.format {
	case "human":
		c.format = "raw"
	case "raw", "json", "yml":
	default:
		return fmt.Errorf("invalid output format: %q (accepted: raw, json, yml)", c.format)
	}
	if reconcile.Sanitize(c.prefix) == "" || strings.ContainsAny(c.prefix, " */?") {
		return fmt.Errorf("--prefix %q isn't usable as an SSH alias prefix", c.prefix)
	}
	return c.init()
}

func (c *config) init() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	c.home = home
	c.ssh = sshconf.DefaultPaths(home)

	if c.identityFile == "" {
		for _, k := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
			p := filepath.Join(home, ".ssh", k)
			if _, err := os.Stat(p); err == nil {
				c.identityFile = p
				break
			}
		}
	} else if strings.HasPrefix(c.identityFile, "~/") {
		c.identityFile = filepath.Join(home, c.identityFile[2:])
	}

	// The Bitrise CLI gives each plugin a data dir; fall back to the usual
	// location when run as a standalone binary.
	dataDir := os.Getenv("BITRISE_PLUGIN_INPUT_DATA_DIR")
	if dataDir == "" {
		dataDir = filepath.Join(home, ".bitrise", "plugins", "herdr", "data")
	}
	c.statePath = filepath.Join(dataDir, "state.json")

	c.rde = &rde.Client{Bin: envOr("BITRISE_HERDR_BITRISE_BIN", "bitrise"), Workspace: c.workspace, Run: runCmd}
	c.herdr = &herdr.Client{Bin: envOr("BITRISE_HERDR_HERDR_BIN", "herdr"), Run: runCmd, RunInteractive: runInteractive}
	c.probe = &remote.Prober{SSHBin: envOr("BITRISE_HERDR_SSH_BIN", "ssh"), Run: runCmd}
	return nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// gather reads all three state sources. If the RDE API fails it returns an
// error before anything else happens, so a network blip never removes
// anything.
func (c *config) gather(ctx context.Context, withHerdr bool) (reconcile.Inputs, error) {
	in := reconcile.Inputs{ViewErrors: map[string]error{}}
	sessions, err := c.rde.List(ctx)
	if err != nil {
		return in, fmt.Errorf("couldn't list RDE sessions, so nothing was changed: %w", err)
	}

	// The list has no SSH details; fetch them for running sessions.
	sem := make(chan struct{}, max(1, c.parallel))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := range sessions {
		if sessions[i].Status != rde.StatusRunning {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			s, err := c.rde.View(ctx, sessions[i].ID)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				in.ViewErrors[sessions[i].ID] = err
				return
			}
			sessions[i] = s
		}()
	}
	wg.Wait()
	in.Sessions = sessions

	if in.SSH, in.Unmanaged, err = c.ssh.Scan(c.prefix); err != nil {
		return in, fmt.Errorf("read %s: %w", c.ssh.Dir, err)
	}
	if in.State, err = state.Load(c.statePath); err != nil {
		return in, fmt.Errorf("read %s: %w", c.statePath, err)
	}
	if withHerdr {
		if in.Machines, err = c.herdr.List(ctx); err != nil {
			return in, err
		}
	}
	return in, nil
}

func (c *config) deps(ctx context.Context, st *state.State) (reconcile.Deps, error) {
	d := reconcile.Deps{
		SSH: c.ssh, Prefix: c.prefix, Herdr: c.herdr, Probe: c.probe,
		State: st, Log: os.Stderr, Parallel: c.parallel,
		CheckUntil: c.waitUntil, CheckPoll: waitPoll,
	}
	s, err := c.herdr.Status(ctx)
	if err != nil {
		return d, err
	}
	d.Local = s
	return d, nil
}

func (c *config) opts(withHerdr bool) reconcile.Options {
	return reconcile.Options{Prefix: c.prefix, KeepTerminated: c.keepTerminated, IdentityFile: c.identityFile, Herdr: withHerdr}
}

func cmdSync(ctx context.Context, c *config, withHerdr bool) (int, error) {
	in, err := c.gather(ctx, withHerdr)
	if err != nil {
		return 1, err
	}
	plan := reconcile.Build(in, c.opts(withHerdr))
	return c.apply(ctx, plan, in.State)
}

func (c *config) apply(ctx context.Context, plan reconcile.Plan, st *state.State) (int, error) {
	d := reconcile.Deps{SSH: c.ssh, Prefix: c.prefix, Herdr: c.herdr, Probe: c.probe, State: st, Log: os.Stderr, Parallel: c.parallel}
	for _, a := range plan.Actions {
		if a.Kind == reconcile.AddHerdr && !c.dryRun {
			var err error
			if d, err = c.deps(ctx, st); err != nil {
				return 1, err
			}
			break
		}
	}
	rep, err := reconcile.Apply(ctx, plan, d, c.dryRun)
	if !c.dryRun {
		if saveErr := st.Save(c.statePath); saveErr != nil && err == nil {
			err = fmt.Errorf("save %s: %w", c.statePath, saveErr)
		}
	}
	c.printReport(rep)
	if err != nil {
		return 1, err
	}
	if rep.Failed() {
		return 1, nil
	}
	if c.failOnSkip && !c.dryRun {
		for _, r := range rep.Results {
			if r.Outcome == reconcile.Skipped {
				return 1, nil
			}
		}
	}
	return 0, nil
}

func (c *config) printReport(rep reconcile.Report) {
	if c.format != "raw" {
		if rep.Results == nil {
			rep.Results = []reconcile.Result{}
		}
		c.writeStructured(rep)
		return
	}
	counts := map[string]int{}
	verb := map[reconcile.Kind]string{
		reconcile.WriteSSH: "ssh written", reconcile.RemoveSSH: "ssh removed",
		reconcile.AddHerdr: "herdr added", reconcile.RemoveHerdr: "herdr removed", reconcile.Skip: "skipped",
	}
	if rep.IncludeChanged {
		state := "Updated"
		if rep.DryRun {
			state = "Would update"
		}
		fmt.Printf("%s %s so its Include line comes before every Host block\n", state, c.ssh.ConfigFile)
	}
	for _, r := range rep.Results {
		mark := map[reconcile.Outcome]string{reconcile.Done: "✓", reconcile.Planned: "→", reconcile.Skipped: "-", reconcile.Failed: "✗"}[r.Outcome]
		session := ""
		if r.Session != "" {
			session = fmt.Sprintf(" (%s)", r.Session)
		}
		fmt.Printf("%s %-14s %s%s: %s\n", mark, r.Action, r.Alias, session, r.Detail)
		key := verb[r.Action]
		if r.Outcome == reconcile.Failed {
			key = "failed"
		} else if r.Outcome == reconcile.Skipped {
			key = "skipped"
		}
		counts[key]++
	}
	var parts []string
	for _, k := range []string{"ssh written", "ssh removed", "herdr added", "herdr removed", "skipped", "failed"} {
		if counts[k] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[k], k))
		}
	}
	prefix := ""
	if rep.DryRun {
		prefix = "Dry run, nothing changed. Planned: "
	}
	if len(parts) == 0 {
		fmt.Println(prefix + "No changes.")
		return
	}
	fmt.Println(prefix + strings.Join(parts, ", ") + ".")
}

// writeStructured prints v as --format json or yml. YAML is built from the
// JSON so both use the same keys in the same order, as the Bitrise CLI does.
func (c *config) writeStructured(v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		return
	}
	if c.format == "json" {
		os.Stdout.Write(append(b, '\n'))
		return
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		return
	}
	blockStyle(&doc)
	enc := yaml.NewEncoder(os.Stdout)
	enc.SetIndent(2)
	_ = enc.Encode(&doc)
}

// blockStyle drops the flow and quoting styles the JSON source gave each
// node, so the encoder picks plain block YAML.
func blockStyle(n *yaml.Node) {
	n.Style = 0
	for _, c := range n.Content {
		blockStyle(c)
	}
}

// resolve finds the listed session an argument refers to: an ID, an alias,
// or an exact name.
func resolve(arg string, sessions []rde.Session, aliases map[string]string) (rde.Session, error) {
	var byName []rde.Session
	for _, s := range sessions {
		if s.ID == arg || aliases[s.ID] == arg {
			return s, nil
		}
		if s.Name == arg {
			byName = append(byName, s)
		}
	}
	switch len(byName) {
	case 1:
		return byName[0], nil
	case 0:
		return rde.Session{}, fmt.Errorf("no session matches %q (try `bitrise :herdr status`)", arg)
	}
	var ids []string
	for _, s := range byName {
		ids = append(ids, s.ID)
	}
	return rde.Session{}, fmt.Errorf("%d sessions are named %q; pass an ID: %s", len(byName), arg, strings.Join(ids, ", "))
}

func cmdAttach(ctx context.Context, c *config, arg string) (int, error) {
	in, s, err := c.attachTarget(ctx, arg)
	if err != nil {
		return 1, err
	}
	if s.Status != rde.StatusRunning {
		return 1, fmt.Errorf("session %q is %s; attach needs a running session", s.Name, s.Status)
	}
	plan := reconcile.Build(in, c.opts(true))
	alias := plan.Aliases[s.ID]
	var acts []reconcile.Action
	for _, a := range plan.Actions {
		if a.Alias != alias || (a.Kind != reconcile.WriteSSH && a.Kind != reconcile.AddHerdr && a.Kind != reconcile.Skip) {
			continue
		}
		if a.Kind == reconcile.AddHerdr && c.label != "" {
			a.Label = c.label
		}
		acts = append(acts, a)
	}
	plan.Actions = acts
	if len(acts) == 0 {
		fmt.Fprintf(os.Stderr, "%s is already attached.\n", alias)
	}
	// A skip means the session isn't attached, so commands chained after
	// attach shouldn't run.
	c.failOnSkip = true
	return c.apply(ctx, plan, in.State)
}

// waitPoll is how often attach --wait re-reads the session.
var waitPoll = 5 * time.Second

// deadStatuses can't turn into running on their own, so attach --wait stops
// on them instead of polling until the timeout.
var deadStatuses = map[string]bool{"terminating": true, rde.StatusTerminated: true, "failed": true}

// attachTarget gathers state and resolves the session to attach. With --wait
// it gathers again until the session is running with SSH open, since
// `bitrise rde session create/restore --wait` returns once the session is
// running but before its SSH server is reachable. The readiness check then
// keeps retrying until the same deadline, while the startup script finishes.
func (c *config) attachTarget(ctx context.Context, arg string) (reconcile.Inputs, rde.Session, error) {
	deadline := time.Now().Add(c.waitTimeout)
	if c.wait {
		c.waitUntil = deadline
	}
	last := ""
	for {
		in, err := c.gather(ctx, true)
		if err != nil {
			return in, rde.Session{}, err
		}
		s, err := resolve(arg, in.Sessions, reconcile.Aliases(in.Sessions, c.prefix, in.SSH))
		if err != nil || !c.wait {
			return in, s, err
		}
		if deadStatuses[s.Status] {
			return in, s, fmt.Errorf("session %q is %s, so it won't become ready", s.Name, s.Status)
		}
		why := s.Status
		if s.Status == rde.StatusRunning {
			switch err := in.ViewErrors[s.ID]; {
			case err != nil:
				why = fmt.Sprintf("running, but its details couldn't be fetched: %v", err)
			case !s.SSHConnectionOpen || s.SSHAddress == "":
				why = "running, SSH not ready yet"
			default:
				return in, s, nil
			}
		}
		if time.Now().After(deadline) {
			return in, s, fmt.Errorf("timed out after %s waiting for session %q (%s)", c.waitTimeout, s.Name, why)
		}
		if why != last {
			fmt.Fprintf(os.Stderr, "Waiting for %s: %s\n", s.Name, why)
			last = why
		}
		select {
		case <-ctx.Done():
			return in, s, ctx.Err()
		case <-time.After(waitPoll):
		}
	}
}

func cmdDetach(ctx context.Context, c *config, arg string) (int, error) {
	managed, _, err := c.ssh.Scan(c.prefix)
	if err != nil {
		return 1, err
	}
	st, err := state.Load(c.statePath)
	if err != nil {
		return 1, err
	}
	machines, err := c.herdr.List(ctx)
	if err != nil {
		return 1, err
	}

	// The session may already be gone, so match local state first and only
	// ask the API to turn a name or ID into an alias.
	alias, sessionName := "", ""
	if _, ok := managed[arg]; ok {
		alias = arg
	}
	for _, m := range machines {
		if alias == "" && m.Target == arg && strings.HasPrefix(arg, c.prefix) {
			alias = arg
		}
	}
	for a, e := range managed {
		if alias == "" && e.SessionID == arg {
			alias = a
		}
	}
	if alias == "" {
		sessions, err := c.rde.List(ctx)
		if err != nil {
			return 1, fmt.Errorf("%q isn't a managed alias, and listing sessions to resolve it failed: %w", arg, err)
		}
		aliases := reconcile.Aliases(sessions, c.prefix, managed)
		s, err := resolve(arg, sessions, aliases)
		if err != nil {
			return 1, err
		}
		alias, sessionName = aliases[s.ID], s.Name
	}

	var acts []reconcile.Action
	var notes []string
	for _, m := range machines {
		if m.Target != alias {
			continue
		}
		if _, hasSSH := managed[alias]; st.Owns(m.ID) || hasSSH {
			acts = append(acts, reconcile.Action{Kind: reconcile.RemoveHerdr, Alias: alias, ProfileID: m.ID, SessionName: sessionName, Reason: "detach"})
		} else {
			notes = append(notes, fmt.Sprintf("Herdr machine %s (%s) wasn't added by this plugin; leaving it. Remove it with `herdr machine remove %s`.", m.Label, m.ID, m.ID))
		}
	}
	if e, ok := managed[alias]; ok {
		acts = append(acts, reconcile.Action{Kind: reconcile.RemoveSSH, Alias: alias, SessionID: e.SessionID, SessionName: sessionName, Entry: e, Reason: "detach"})
	}
	for _, n := range notes {
		fmt.Fprintln(os.Stderr, n)
	}
	if len(acts) == 0 {
		fmt.Fprintf(os.Stderr, "Nothing to detach for %s.\n", alias)
		return 0, nil
	}
	if !c.dryRun && !c.yes && isTerminal(os.Stdin) {
		if !confirm(fmt.Sprintf("Remove the Herdr machine, SSH entry and host key for %s? The RDE session itself isn't touched.", alias)) {
			return 1, errors.New("cancelled")
		}
	}
	return c.apply(ctx, reconcile.Plan{Actions: acts}, st)
}

type statusRow struct {
	Alias     string `json:"alias"`
	SessionID string `json:"session_id,omitempty"`
	Session   string `json:"session,omitempty"`
	State     string `json:"state"`
	SSH       string `json:"ssh"`
	Herdr     string `json:"herdr"`
	HerdrID   string `json:"herdr_id,omitempty"`
	Drift     string `json:"drift,omitempty"`
	Unmanaged bool   `json:"unmanaged,omitempty"`
}

func cmdStatus(ctx context.Context, c *config) (int, error) {
	in, err := c.gather(ctx, true)
	if err != nil {
		return 1, err
	}
	plan := reconcile.Build(in, c.opts(true))
	if len(in.SSH) > 0 {
		if missing, err := c.ssh.EnsureInclude(c.prefix, true); err == nil && missing {
			fmt.Fprintf(os.Stderr, "Warning: %s doesn't include %s before its first Host block, so the aliases below won't resolve. Run `bitrise :herdr sync` to fix it.\n\n",
				c.ssh.ConfigFile, c.ssh.IncludePattern(c.prefix))
		}
	}

	byTarget := map[string]herdr.Machine{}
	for _, m := range in.Machines {
		byTarget[m.Target] = m
	}
	pending := map[string][]string{}
	for _, a := range plan.Actions {
		if a.Kind == reconcile.Skip {
			pending[a.Alias] = append(pending[a.Alias], "skipped: "+a.Reason)
		} else {
			pending[a.Alias] = append(pending[a.Alias], string(a.Kind))
		}
	}

	rows := map[string]*statusRow{}
	row := func(alias string) *statusRow {
		if r, ok := rows[alias]; ok {
			return r
		}
		r := &statusRow{Alias: alias, State: "gone", SSH: "missing", Herdr: "missing"}
		rows[alias] = r
		return r
	}
	for _, s := range in.Sessions {
		r := row(plan.Aliases[s.ID])
		r.SessionID, r.Session, r.State = s.ID, s.Name, s.Status
	}
	for a, e := range in.SSH {
		r := row(a)
		r.SSH = "ok"
		if r.SessionID != "" && r.SessionID != e.SessionID {
			r.SSH = "stale"
		}
	}
	for a := range in.Unmanaged {
		row(a).SSH = "unmanaged"
	}
	for _, m := range in.Machines {
		if !strings.HasPrefix(m.Target, c.prefix) {
			continue
		}
		r := row(m.Target)
		r.HerdrID = m.ID
		_, hasSSH := in.SSH[m.Target]
		if in.State.Owns(m.ID) || hasSSH {
			r.Herdr = "ok"
		} else {
			r.Herdr = "unmanaged"
			r.Unmanaged = true
		}
	}
	for a, r := range rows {
		r.Drift = strings.Join(pending[a], ", ")
		if r.SessionID == "" && r.SSH != "ok" && r.Herdr != "ok" {
			r.State = "-" // only hand-made entries; not ours to judge
		}
	}

	var list []statusRow
	for _, a := range sshconf.SortedAliases(rows) {
		list = append(list, *rows[a])
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].State == "running" && list[j].State != "running" })

	if c.format != "raw" {
		if list == nil {
			list = []statusRow{}
		}
		c.writeStructured(list)
		return 0, nil
	}
	if len(list) == 0 {
		fmt.Println("No RDE sessions, SSH entries or Herdr machines.")
		return 0, nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ALIAS\tSESSION\tSTATE\tSSH\tHERDR\tDRIFT")
	for _, r := range list {
		drift := r.Drift
		if drift == "" {
			drift = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Alias, orDash(r.Session), r.State, r.SSH, r.Herdr, drift)
	}
	tw.Flush()
	if plan.Changes() > 0 {
		fmt.Printf("\n%d change(s) pending; run `bitrise :herdr sync` to apply them.\n", plan.Changes())
	}
	return 0, nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
