package main

// End-to-end tests against fake bitrise, herdr and ssh binaries in a
// throwaway HOME, so the developer's own SSH config is never touched. They
// follow the v1 acceptance criteria.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benbitrise/bitrise-herdr-plugin/internal/reconcile"
)

const fakeBitrise = `#!/bin/sh
[ -f "$HOME/api-down" ] && { echo "RDE API 503: unavailable" >&2; exit 1; }
case "$3" in
  list)
    # Each list call first moves in the next stage-N dir, if any, so a test
    # can script a session coming up across polls.
    for d in "$HOME"/stage-*; do [ -d "$d" ] && { mv "$d"/* "$HOME"/; rmdir "$d"; break; }; done
    cat "$HOME/list.json" ;;
  view) cat "$HOME/view-$4.json" ;;
  *) exit 2 ;;
esac
`

// Machines are stored one per line as id|target|label.
const fakeHerdr = `#!/bin/sh
db="$HOME/herdr-machines"
touch "$db"
case "$1 $2" in
  "status client") echo '{"version":"0.9.3","protocol":22,"endpoint_protocol_generation":1,"endpoint_capabilities":["surface_interest","presentation_effects_fence","health_check"]}' ;;
  "machine list")
    awk -F'|' 'BEGIN{printf "["} {if (NR>1) printf ","; printf "{\"id\":\"%s\",\"target\":\"%s\",\"label\":\"%s\",\"session\":\"default\",\"enabled\":true}", $1, $2, $3} END{print "]"}' "$db" ;;
  "machine add")
    target="$3"; shift 3; label="$target"
    while [ $# -gt 0 ]; do [ "$1" = "--label" ] && label="$2"; shift; done
    echo "p$(wc -l < "$db" | tr -d ' ')-$target|$target|$label" >> "$db" ;;
  "machine remove")
    grep -v "^$3|" "$db" > "$db.tmp"; mv "$db.tmp" "$db" ;;
  *) exit 2 ;;
esac
`

const fakeSSH = `#!/bin/sh
if [ "$1" = "-G" ]; then
  f="$HOME/.ssh/config.d/$2"
  if [ -f "$f" ]; then awk '$1=="HostName"{print "hostname " $2} $1=="Port"{print "port " $2}' "$f"
  else echo "hostname $2"; echo "port 22"; fi
  exit 0
fi
for a; do target="$prev"; prev="$a"; done
last="$a"
case "$last" in
  *NI_HERDR*)
    st='{"version":"0.9.3","protocol":22,"endpoint_protocol_generation":1,"endpoint_capabilities":["surface_interest","presentation_effects_fence","health_check"]}'
    [ -f "$HOME/remote-status-$target" ] && st=$(cat "$HOME/remote-status-$target")
    echo "NI_HERDR=/opt/homebrew/bin/herdr"; echo "NI_CLAUDE=/u/claude"
    # installing-<alias> holds how many more checks see herdr missing, like
    # a startup script that hasn't installed it yet.
    h=/opt/homebrew/bin/herdr
    if [ -f "$HOME/installing-$target" ]; then
      n=$(cat "$HOME/installing-$target"); echo $((n-1)) > "$HOME/installing-$target"
      [ "$n" -gt 0 ] && h=
    fi
    echo "L_HERDR=$h"; echo "L_CLAUDE=/u/claude"
    echo L_STATUS_BEGIN; echo "$st"; echo L_STATUS_END ;;
esac
exit 0
`

type fakeSession struct {
	ID, Name, Status, Host string
	Port                   int
}

type env struct {
	t    *testing.T
	home string
}

func setup(t *testing.T) *env {
	t.Helper()
	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	os.MkdirAll(bin, 0o755)
	for name, script := range map[string]string{"bitrise": fakeBitrise, "herdr": fakeHerdr, "ssh": fakeSSH} {
		os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755)
	}
	t.Setenv("HOME", home)
	t.Setenv("BITRISE_HERDR_BITRISE_BIN", filepath.Join(bin, "bitrise"))
	t.Setenv("BITRISE_HERDR_HERDR_BIN", filepath.Join(bin, "herdr"))
	t.Setenv("BITRISE_HERDR_SSH_BIN", filepath.Join(bin, "ssh"))
	t.Setenv("BITRISE_PLUGIN_INPUT_DATA_DIR", filepath.Join(home, "plugin-data"))
	return &env{t, home}
}

func (e *env) sessions(ss ...fakeSession) { e.sessionsIn("", ss...) }

// sessionsIn writes the fake API's responses into dir under HOME. A session
// with Port 0 is running but doesn't accept SSH yet.
func (e *env) sessionsIn(dir string, ss ...fakeSession) {
	dir = filepath.Join(e.home, dir)
	os.MkdirAll(dir, 0o755)
	var items []map[string]any
	for _, s := range ss {
		items = append(items, map[string]any{"id": s.ID, "name": s.Name, "status": s.Status, "owner_type": "user"})
		view, _ := json.Marshal(map[string]any{
			"id": s.ID, "name": s.Name, "status": s.Status, "ssh_connection_open": s.Port != 0,
			"ssh_address": fmt.Sprintf("ssh vagrant@%s -p %d", s.Host, s.Port),
		})
		os.WriteFile(filepath.Join(dir, "view-"+s.ID+".json"), view, 0o644)
	}
	list, _ := json.Marshal(map[string]any{"items": items})
	os.WriteFile(filepath.Join(dir, "list.json"), list, 0o644)
}

func (e *env) write(rel, content string) {
	p := filepath.Join(e.home, rel)
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(p, []byte(content), 0o600)
}

func (e *env) read(rel string) string {
	b, _ := os.ReadFile(filepath.Join(e.home, rel))
	return string(b)
}

func (e *env) exists(rel string) bool {
	_, err := os.Stat(filepath.Join(e.home, rel))
	return err == nil
}

// run runs a command with --format json and returns its exit code and report.
func (e *env) run(args ...string) (int, reconcile.Report) {
	e.t.Helper()
	r, w, _ := os.Pipe()
	stdout, stderr := os.Stdout, os.Stderr
	os.Stdout = w
	devnull, _ := os.Open(os.DevNull)
	os.Stderr = devnull
	code, err := run(context.Background(), append(args, "--format", "json", "--yes"))
	w.Close()
	os.Stdout, os.Stderr = stdout, stderr
	out, _ := io.ReadAll(r)
	if err != nil && code == 0 {
		code = 1
	}
	var rep reconcile.Report
	json.Unmarshal(out, &rep)
	return code, rep
}

func changes(rep reconcile.Report) []string {
	var out []string
	for _, r := range rep.Results {
		out = append(out, fmt.Sprintf("%s %s %s", r.Action, r.Alias, r.Outcome))
	}
	return out
}

var three = []fakeSession{
	{"aaaaaaaa-1", "ticket 101", "running", "h1.example", 1001},
	{"bbbbbbbb-2", "ticket 102", "running", "h2.example", 1002},
	{"cccccccc-3", "ticket 103", "running", "h3.example", 1003},
}

func TestEndToEnd(t *testing.T) {
	e := setup(t)
	e.sessions(three...)
	// A pre-existing config with a Host block, and a hand-made rde-mac1
	// entry and Herdr machine that must survive every sync.
	userConfig := "Host work\n  HostName w.example\n  User me\n"
	e.write(".ssh/config", userConfig)
	handMade := "Host rde-mac1\n  HostName old.example\n  Port 28985\n"
	e.write(".ssh/config.d/rde-mac1", handMade)
	e.write("herdr-machines", "hand|rde-mac1|rde-mac1\n")

	// --dry-run plans exactly what the real run then does.
	code, dry := e.run("sync", "--dry-run")
	if code != 0 || !dry.DryRun || !dry.IncludeChanged {
		t.Fatalf("dry run: code=%d %+v", code, dry)
	}
	if e.exists(".ssh/config.d/rde-ticket-101") {
		t.Fatal("dry run wrote a file")
	}

	code, rep := e.run("sync")
	if code != 0 {
		t.Fatalf("sync failed: %v", changes(rep))
	}
	if len(rep.Results) != len(dry.Results) {
		t.Fatalf("dry run planned %v, real run did %v", changes(dry), changes(rep))
	}
	for i := range rep.Results {
		if rep.Results[i].Action != dry.Results[i].Action || rep.Results[i].Alias != dry.Results[i].Alias || rep.Results[i].Outcome != reconcile.Done {
			t.Fatalf("dry run planned %v, real run did %v", changes(dry), changes(rep))
		}
	}
	for _, a := range []string{"rde-ticket-101", "rde-ticket-102", "rde-ticket-103"} {
		if !strings.Contains(e.read(".ssh/config.d/"+a), "Host "+a+"\n") {
			t.Fatalf("missing SSH entry %s", a)
		}
		if !strings.Contains(e.read("herdr-machines"), "|"+a+"|") {
			t.Fatalf("missing Herdr machine %s", a)
		}
	}
	cfg := e.read(".ssh/config")
	if strings.Index(cfg, "Include ") > strings.Index(cfg, "Host work") || !strings.Contains(cfg, userConfig) {
		t.Fatalf("config: %q", cfg)
	}

	// A second sync makes no changes.
	if code, rep := e.run("sync"); code != 0 || len(rep.Results) != 0 || rep.IncludeChanged {
		t.Fatalf("second sync: %v", changes(rep))
	}

	// A reset ~/.ssh/config gets its Include line back, even with nothing
	// else to change.
	e.write(".ssh/config", userConfig)
	if code, rep := e.run("sync"); code != 0 || len(rep.Results) != 0 || !rep.IncludeChanged || !strings.Contains(e.read(".ssh/config"), "Include ") {
		t.Fatalf("include not restored: %v %q", changes(rep), e.read(".ssh/config"))
	}

	// Terminating one session removes only its profile, entry and host key.
	key := filepath.Join(e.home, "key")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v %s", err, out)
	}
	pub := strings.Fields(e.read("key.pub"))
	e.write(".ssh/known_hosts_rde",
		"[h1.example]:1001 "+pub[0]+" "+pub[1]+"\n[h2.example]:1002 "+pub[0]+" "+pub[1]+"\n")
	gone := three[1]
	gone.Status = "terminated"
	e.sessions(three[0], gone, three[2])
	code, rep = e.run("sync")
	want := []string{"remove-herdr rde-ticket-102 done", "remove-ssh rde-ticket-102 done"}
	if code != 0 || strings.Join(changes(rep), ",") != strings.Join(want, ",") {
		t.Fatalf("after terminate: %v", changes(rep))
	}
	if e.exists(".ssh/config.d/rde-ticket-102") || strings.Contains(e.read("herdr-machines"), "rde-ticket-102") {
		t.Fatal("terminated session's entry or machine left behind")
	}
	kh := e.read(".ssh/known_hosts_rde")
	if strings.Contains(kh, "h2.example") || !strings.Contains(kh, "h1.example") {
		t.Fatalf("known_hosts_rde: %q", kh)
	}
	if e.read(".ssh/config.d/rde-mac1") != handMade || !strings.Contains(e.read("herdr-machines"), "hand|rde-mac1|") {
		t.Fatal("hand-made entry or machine was modified")
	}

	// With the API down, sync fails and changes nothing.
	before := e.read("herdr-machines") + e.read(".ssh/config")
	e.write("api-down", "")
	e.sessions()
	if code, _ := e.run("sync"); code == 0 {
		t.Fatal("sync should fail when the RDE API is unreachable")
	}
	if e.read("herdr-machines")+e.read(".ssh/config") != before || !e.exists(".ssh/config.d/rde-ticket-101") {
		t.Fatal("sync changed things while the API was down")
	}
	os.Remove(filepath.Join(e.home, "api-down"))

	// A box whose herdr speaks another endpoint generation is skipped with the
	// template fix; a different release on the same generation is added.
	skew := fakeSession{"dddddddd-4", "skewed", "running", "h4.example", 1004}
	e.write("remote-status-rde-skewed", `{"version":"0.8.0","protocol":20}`)
	older := fakeSession{"eeeeeeee-5", "older", "running", "h5.example", 1005}
	e.write("remote-status-rde-older", `{"version":"0.9.1","protocol":21,"endpoint_protocol_generation":1,"endpoint_capabilities":["surface_interest","presentation_effects_fence","health_check"]}`)
	e.sessions(three[0], three[2], skew, older)
	code, rep = e.run("sync")
	var skipped *reconcile.Result
	for i, r := range rep.Results {
		if r.Alias == "rde-skewed" && r.Action == reconcile.AddHerdr {
			skipped = &rep.Results[i]
		}
	}
	if code != 0 || skipped == nil || skipped.Outcome != reconcile.Skipped || !strings.Contains(skipped.Detail, "Template fix: install herdr 0.9.3") {
		t.Fatalf("version skew: %v %+v", changes(rep), skipped)
	}
	if strings.Contains(e.read("herdr-machines"), "rde-skewed") {
		t.Fatal("skewed box was added")
	}
	if !strings.Contains(e.read("herdr-machines"), "rde-older") {
		t.Fatalf("compatible older herdr wasn't added: %v", changes(rep))
	}

	// detach removes one session's machine and SSH entry; attach adds it back.
	if code, rep := e.run("detach", "ticket 101"); code != 0 || len(rep.Results) != 2 {
		t.Fatalf("detach: %v", changes(rep))
	}
	if e.exists(".ssh/config.d/rde-ticket-101") || strings.Contains(e.read("herdr-machines"), "rde-ticket-101") {
		t.Fatal("detach left things behind")
	}
	if code, rep := e.run("attach", "ticket 101", "--label", "T-101"); code != 0 || len(rep.Results) != 2 {
		t.Fatalf("attach: %v", changes(rep))
	}
	if !strings.Contains(e.read("herdr-machines"), "|rde-ticket-101|T-101") {
		t.Fatalf("attach label: %q", e.read("herdr-machines"))
	}
}

func TestAttachWait(t *testing.T) {
	waitPoll = time.Millisecond
	e := setup(t)
	box := fakeSession{"dddddddd-4", "ticket 104", "starting", "h4.example", 1004}

	// Without --wait, a session that isn't running yet is an error.
	e.sessions(box)
	if code, _ := e.run("attach", "ticket 104"); code == 0 {
		t.Fatal("attach without --wait should fail on a starting session")
	}

	// With --wait it polls through starting and running-without-SSH.
	notOpen := box
	notOpen.Status, notOpen.Port = "running", 0
	up := box
	up.Status = "running"
	e.sessionsIn("stage-1", box)
	e.sessionsIn("stage-2", notOpen)
	e.sessionsIn("stage-3", up)
	e.write("installing-rde-ticket-104", "2")
	code, rep := e.run("attach", "ticket 104", "--wait")
	if code != 0 || strings.Join(changes(rep), ",") != "write-ssh rde-ticket-104 done,add-herdr rde-ticket-104 done" {
		t.Fatalf("attach --wait: code=%d %v", code, changes(rep))
	}
	if e.exists("stage-3") || e.read("installing-rde-ticket-104") != "-1\n" {
		t.Fatal("attach --wait returned before the session was ready")
	}

	// Without --wait a box that isn't ready yet is skipped, and attach exits
	// non-zero so a chained command doesn't run.
	e.run("detach", "ticket 104")
	e.write("installing-rde-ticket-104", "1")
	if code, rep := e.run("attach", "ticket 104"); code == 0 || len(rep.Results) != 2 || rep.Results[1].Outcome != reconcile.Skipped {
		t.Fatalf("attach on an unready box: code=%d %v", code, changes(rep))
	}

	// With --wait, a box that never becomes ready fails at the timeout.
	e.run("detach", "ticket 104")
	e.write("installing-rde-ticket-104", "1000")
	if code, rep := e.run("attach", "ticket 104", "--wait", "--wait-timeout", "50ms"); code == 0 || strings.Contains(e.read("herdr-machines"), "rde-ticket-104") {
		t.Fatalf("attach --wait on a never-ready box: code=%d %v", code, changes(rep))
	}

	// It gives up at the timeout, and at once on a session that can't come up.
	stuck := box
	stuck.ID, stuck.Name = "eeeeeeee-5", "ticket 105"
	failed := box
	failed.ID, failed.Name, failed.Status = "ffffffff-6", "ticket 106", "failed"
	e.sessions(up, stuck, failed)
	if code, _ := e.run("attach", "ticket 105", "--wait", "--wait-timeout", "20ms"); code == 0 {
		t.Fatal("attach --wait should time out on a session that stays starting")
	}
	start := time.Now()
	if code, _ := e.run("attach", "ticket 106", "--wait"); code == 0 || time.Since(start) > 5*time.Second {
		t.Fatal("attach --wait should fail at once on a failed session")
	}
}
