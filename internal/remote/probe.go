// Package remote checks a session over SSH: that the alias resolves, that
// the box accepts the connection, and that it has what Herdr needs.
package remote

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/benbitrise/bitrise-herdr-plugin/internal/herdr"
)

type Runner func(ctx context.Context, name string, args ...string) (stdout, stderr []byte, err error)

type Prober struct {
	SSHBin string
	Run    Runner
}

// Kind classifies an SSH failure, so a config-loading bug isn't reported as
// an auth problem.
type Kind string

const (
	OK      Kind = "ok"
	Config  Kind = "config"   // the alias doesn't resolve to the session
	HostKey Kind = "host-key" // stale key for a reused hostname
	Auth    Kind = "auth"     // the box rejected the key
	Network Kind = "network"  // unreachable, timed out or refused
	Unknown Kind = "unknown"
)

type Failure struct {
	Kind   Kind
	Detail string
}

func (f *Failure) Error() string { return fmt.Sprintf("%s: %s", f.Kind, f.Detail) }

// CheckConfig runs `ssh -G alias` and confirms ssh resolves the alias to
// the session's host and port. If it doesn't, the Include isn't being read.
func (p *Prober) CheckConfig(ctx context.Context, alias, host string, port int) error {
	out, errOut, err := p.Run(ctx, p.SSHBin, "-G", alias)
	if err != nil {
		return &Failure{Config, fmt.Sprintf("ssh -G %s failed: %s", alias, strings.TrimSpace(string(errOut)))}
	}
	var gotHost string
	var gotPort int
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Fields(l)
		if len(f) != 2 {
			continue
		}
		switch f[0] {
		case "hostname":
			gotHost = f[1]
		case "port":
			gotPort, _ = strconv.Atoi(f[1])
		}
	}
	if !strings.EqualFold(gotHost, host) || gotPort != port {
		return &Failure{Config, fmt.Sprintf(
			"`ssh -G %s` resolves to %s:%d, not %s:%d, so ssh isn't reading the generated file. "+
				"Check that the Include line is above every Host block in ~/.ssh/config",
			alias, gotHost, gotPort, host, port)}
	}
	return nil
}

func (p *Prober) sshArgs(alias string, cmd ...string) []string {
	return append([]string{"-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", alias}, cmd...)
}

// Reach connects without prompting and runs `true`.
func (p *Prober) Reach(ctx context.Context, alias string) error {
	_, errOut, err := p.Run(ctx, p.SSHBin, p.sshArgs(alias, "true")...)
	if err == nil {
		return nil
	}
	return classify(string(errOut))
}

func classify(stderr string) *Failure {
	s := strings.TrimSpace(stderr)
	low := strings.ToLower(s)
	last := s
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		last = s[i+1:]
	}
	switch {
	case strings.Contains(low, "host key verification failed"),
		strings.Contains(low, "remote host identification has changed"):
		return &Failure{HostKey, last}
	case strings.Contains(low, "permission denied"):
		return &Failure{Auth, last}
	case strings.Contains(low, "could not resolve hostname"),
		strings.Contains(low, "timed out"),
		strings.Contains(low, "connection refused"),
		strings.Contains(low, "no route to host"),
		strings.Contains(low, "network is unreachable"),
		strings.Contains(low, "connection closed"),
		strings.Contains(low, "connection reset"):
		return &Failure{Network, last}
	}
	if last == "" {
		last = "ssh failed with no output"
	}
	return &Failure{Unknown, last}
}

// Health is what the box reports about the tools Herdr needs. NonInteractive*
// are resolved in the shell ssh runs commands in; Login* in a login shell,
// which is what Herdr itself uses to start its server.
type Health struct {
	NonInteractiveHerdr  string
	NonInteractiveClaude string
	LoginHerdr           string
	LoginClaude          string
	Herdr                *herdr.Status // login-shell herdr's status; nil if it reported none
}

// healthScript prints KEY=value lines. The single-quoted part runs in the
// user's login shell.
const healthScript = `echo "NI_HERDR=$(command -v herdr)"; echo "NI_CLAUDE=$(command -v claude)"; ` +
	`exec "${SHELL:-/bin/sh}" -lc 'echo "L_HERDR=$(command -v herdr)"; echo "L_CLAUDE=$(command -v claude)"; echo L_STATUS_BEGIN; herdr status client --json 2>/dev/null; echo; echo L_STATUS_END'`

func (p *Prober) Health(ctx context.Context, alias string) (Health, error) {
	out, errOut, err := p.Run(ctx, p.SSHBin, p.sshArgs(alias, healthScript)...)
	if err != nil {
		return Health{}, classify(string(errOut))
	}
	return parseHealth(string(out)), nil
}

func parseHealth(out string) Health {
	var h Health
	var status []string
	inStatus := false
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		switch {
		case l == "L_STATUS_BEGIN":
			inStatus = true
			continue
		case l == "L_STATUS_END":
			inStatus = false
			continue
		case inStatus:
			status = append(status, l)
			continue
		}
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "NI_HERDR":
			h.NonInteractiveHerdr = v
		case "NI_CLAUDE":
			h.NonInteractiveClaude = v
		case "L_HERDR":
			h.LoginHerdr = v
		case "L_CLAUDE":
			h.LoginClaude = v
		}
	}
	if s, ok := herdr.ParseStatus(strings.Join(status, "\n")); ok {
		h.Herdr = &s
	}
	return h
}
