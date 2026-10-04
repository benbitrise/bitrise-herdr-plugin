// Package rde reads Bitrise RDE sessions through the `bitrise rde` CLI's JSON
// output. The CLI documents its --format json shapes as stable, so this is the
// only contract the plugin depends on.
package rde

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const (
	StatusRunning    = "running"
	StatusTerminated = "terminated"
)

// Session is the subset of the CLI's session JSON the plugin uses. List
// results leave the SSH fields empty; View fills them in.
type Session struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Status            string `json:"status"`
	TemplateName      string `json:"template_name"`
	OwnerType         string `json:"owner_type"`
	SSHAddress        string `json:"ssh_address"`
	SSHConnectionOpen bool   `json:"ssh_connection_open"`
}

// Target is where a session's SSH server listens.
type Target struct {
	User string
	Host string
	Port int
}

// Runner runs a command and returns its stdout and stderr.
type Runner func(ctx context.Context, name string, args ...string) (stdout, stderr []byte, err error)

type Client struct {
	Bin       string // path to the bitrise CLI
	Workspace string // optional; the CLI resolves it otherwise
	Run       Runner
}

func (c *Client) run(ctx context.Context, args ...string) ([]byte, error) {
	args = append(args, "--format", "json", "--ci")
	if c.Workspace != "" {
		args = append(args, "--workspace", c.Workspace)
	}
	out, errOut, err := c.Run(ctx, c.Bin, args...)
	if err != nil {
		msg := strings.TrimSpace(string(errOut))
		if msg == "" {
			msg = strings.TrimSpace(string(out))
		}
		return nil, fmt.Errorf("%s %s: %w: %s", c.Bin, strings.Join(args[:3], " "), err, msg)
	}
	return out, nil
}

// List returns the current user's sessions in the workspace.
func (c *Client) List(ctx context.Context) ([]Session, error) {
	out, err := c.run(ctx, "rde", "session", "list")
	if err != nil {
		return nil, err
	}
	var resp struct {
		Items *[]Session `json:"items"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("parse session list: %w", err)
	}
	// A missing items key means the output shape changed. Treat that as a
	// failure, not as "no sessions", so sync never wipes everything.
	if resp.Items == nil {
		return nil, fmt.Errorf("parse session list: no items field in output")
	}
	return *resp.Items, nil
}

// View returns one session with its SSH connection details.
func (c *Client) View(ctx context.Context, id string) (Session, error) {
	out, err := c.run(ctx, "rde", "session", "view", id)
	if err != nil {
		return Session{}, err
	}
	var s Session
	if err := json.Unmarshal(out, &s); err != nil {
		return Session{}, fmt.Errorf("parse session %s: %w", id, err)
	}
	return s, nil
}

var (
	userHostRe = regexp.MustCompile(`([A-Za-z0-9._-]+)@([A-Za-z0-9.-]+)`)
	portRe     = regexp.MustCompile(`(?:^|\s)-p\s*(\d+)`)
)

// ParseSSHAddress parses the backend's ssh_address, e.g.
// "ssh vagrant@vm-….remote-access.bitrise.io -p 28985". It mirrors the CLI's
// own parser: the last user@host wins, and a missing user is an error because
// macOS (vagrant) and Linux (ubuntu) use different accounts.
func ParseSSHAddress(addr string) (Target, error) {
	m := userHostRe.FindAllStringSubmatch(addr, -1)
	if len(m) == 0 {
		return Target{}, fmt.Errorf("unable to parse ssh address %q (no user@host)", addr)
	}
	last := m[len(m)-1]
	t := Target{User: last[1], Host: last[2], Port: 22}
	if pm := portRe.FindStringSubmatch(addr); pm != nil {
		p, err := strconv.Atoi(pm[1])
		if err != nil || p <= 0 || p > 65535 {
			return Target{}, fmt.Errorf("ssh address %q: invalid port %q", addr, pm[1])
		}
		t.Port = p
	}
	return t, nil
}
