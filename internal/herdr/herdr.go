// Package herdr wraps the herdr CLI's machine commands. Herdr is AGPL, so it
// is only ever called as an external binary.
package herdr

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

type Machine struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Target  string `json:"target"`
	Session string `json:"session"`
	Enabled bool   `json:"enabled"`
}

type Runner func(ctx context.Context, name string, args ...string) (stdout, stderr []byte, err error)

// InteractiveRunner runs a command attached to the user's terminal.
type InteractiveRunner func(ctx context.Context, name string, args ...string) error

type Client struct {
	Bin            string
	Run            Runner
	RunInteractive InteractiveRunner
}

func (c *Client) List(ctx context.Context) ([]Machine, error) {
	out, errOut, err := c.Run(ctx, c.Bin, "machine", "list", "--json")
	if err != nil {
		return nil, fmt.Errorf("herdr machine list: %w: %s", err, strings.TrimSpace(string(errOut)))
	}
	var ms []Machine
	if err := json.Unmarshal(out, &ms); err != nil {
		return nil, fmt.Errorf("parse herdr machine list: %w", err)
	}
	return ms, nil
}

// Add registers an SSH target as a machine using the box's default Herdr
// session. machine add may still ask before installing or replacing the
// remote server, so it runs attached to the terminal.
func (c *Client) Add(ctx context.Context, target, label string) error {
	args := []string{"machine", "add", target, "--remote-session", "default"}
	if label != "" {
		args = append(args, "--label", label)
	}
	if err := c.RunInteractive(ctx, c.Bin, args...); err != nil {
		return fmt.Errorf("herdr %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func (c *Client) Remove(ctx context.Context, id string) error {
	_, errOut, err := c.Run(ctx, c.Bin, "machine", "remove", id)
	if err != nil {
		return fmt.Errorf("herdr machine remove %s: %w: %s", id, err, strings.TrimSpace(string(errOut)))
	}
	return nil
}

// Status is what `herdr status client --json` reports about a herdr binary.
type Status struct {
	Version              string   `json:"version"`
	Protocol             int      `json:"protocol"`
	EndpointGeneration   *int     `json:"endpoint_protocol_generation"`
	EndpointCapabilities []string `json:"endpoint_capabilities"`
}

func (c *Client) Status(ctx context.Context) (Status, error) {
	out, errOut, err := c.Run(ctx, c.Bin, "status", "client", "--json")
	if err != nil {
		return Status{}, fmt.Errorf("herdr status client: %w: %s", err, strings.TrimSpace(string(errOut)))
	}
	s, ok := ParseStatus(string(out))
	if !ok || s.EndpointGeneration == nil {
		return Status{}, fmt.Errorf("this laptop's herdr doesn't report an endpoint protocol generation; update herdr")
	}
	return s, nil
}

// ParseStatus reads the last JSON line of `herdr status client --json`, the
// way herdr reads it when probing a remote binary.
func ParseStatus(out string) (Status, bool) {
	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var s Status
		if json.Unmarshal([]byte(strings.TrimSpace(lines[i])), &s) != nil {
			continue
		}
		if s.Version != "" || s.Protocol != 0 || s.EndpointGeneration != nil || len(s.EndpointCapabilities) > 0 {
			return s, true
		}
	}
	return Status{}, false
}

// savedMachineCapabilities are the endpoint capabilities machine add requires
// of the box's herdr (herdr 0.9.3, supports_endpoint_requirement).
var savedMachineCapabilities = []string{"surface_interest", "presentation_effects_fence", "health_check"}

// Incompatibility says why machine add would offer to replace the box's herdr,
// or "" if it would use it as is. herdr compares the endpoint protocol
// generation and capabilities, not release versions.
func Incompatibility(local Status, box *Status) string {
	if box == nil || box.EndpointGeneration == nil {
		return "doesn't report an endpoint protocol generation"
	}
	if local.EndpointGeneration != nil && *box.EndpointGeneration != *local.EndpointGeneration {
		return fmt.Sprintf("speaks endpoint generation %d but this laptop speaks %d", *box.EndpointGeneration, *local.EndpointGeneration)
	}
	var missing []string
	for _, c := range savedMachineCapabilities {
		if !slices.Contains(box.EndpointCapabilities, c) {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		return "lacks " + strings.Join(missing, ", ")
	}
	return ""
}
