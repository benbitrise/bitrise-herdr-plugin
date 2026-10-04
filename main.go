// Command bitrise-herdr-plugin is a Bitrise CLI plugin (`bitrise :herdr`)
// that keeps a laptop's SSH config and Herdr machine list in step with the
// user's running RDE sessions.
package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var version = "dev"

const long = `Keep Herdr machines and SSH config in step with your Bitrise RDE sessions.

The SSH alias and Herdr target for a session are <prefix><sanitized session name>.
The plugin never creates, terminates or deletes RDE sessions, and never touches
SSH files or Herdr machines it didn't create.`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	code, err := run(ctx, os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// run executes one command line. A command that ran sets its own exit code;
// one cobra rejected before running (bad flag, wrong arguments) exits 2.
func run(ctx context.Context, args []string) (int, error) {
	code := -1
	root := newRootCmd(ctx, &config{}, &code)
	root.SetArgs(args)
	err := root.Execute()
	if code < 0 {
		if err != nil {
			return 2, err
		}
		return 0, nil // help or version
	}
	return code, err
}

func newRootCmd(ctx context.Context, c *config, code *int) *cobra.Command {
	// exit adapts a command returning an exit code to cobra's RunE.
	exit := func(fn func(args []string) (int, error)) func(*cobra.Command, []string) error {
		return func(_ *cobra.Command, args []string) (err error) {
			*code, err = fn(args)
			return err
		}
	}
	keepTerminated := func(cmd *cobra.Command) *cobra.Command {
		cmd.Flags().BoolVar(&c.keepTerminated, "keep-terminated", false, "keep entries for terminated sessions, since a restore brings the disk back")
		return cmd
	}

	root := &cobra.Command{
		Use:               "herdr",
		Short:             "Keep Herdr machines and SSH config in step with your Bitrise RDE sessions",
		Long:              long,
		Version:           version,
		Annotations:       map[string]string{cobra.CommandDisplayNameAnnotation: "bitrise :herdr"},
		SilenceUsage:      true,
		SilenceErrors:     true,
		PersistentPreRunE: func(*cobra.Command, []string) error { return c.validate() },
	}
	root.SetVersionTemplate("{{.Version}}\n")
	// Completions would be generated for `herdr`, clashing with the real herdr CLI.
	root.CompletionOptions.DisableDefaultCmd = true
	f := root.PersistentFlags()
	f.StringVar(&c.workspace, "workspace", "", "RDE workspace (default: BITRISE_WORKSPACE_ID or the CLI's default_workspace_id)")
	f.StringVar(&c.prefix, "prefix", "rde-", "SSH alias / Herdr target prefix")
	// Same as the Bitrise CLI: -f/--format per command, -o/--output global.
	f.StringVarP(&c.format, "format", "f", "raw", "output format: raw, json or yml")
	f.StringVarP(&c.format, "output", "o", "raw", "same as --format")
	f.BoolVar(&c.dryRun, "dry-run", false, "print the planned changes without applying them")
	f.BoolVar(&c.yes, "yes", false, "don't ask for confirmation (detach)")
	f.StringVar(&c.identityFile, "identity-file", "", "IdentityFile for generated entries (default: first of ~/.ssh/id_ed25519, id_ecdsa, id_rsa)")
	f.IntVar(&c.parallel, "parallel", 4, "sessions checked at once")

	sync := keepTerminated(&cobra.Command{
		Use:   "sync",
		Short: "SSH entries + Herdr machines for every running session; removes ones for gone sessions",
		Args:  cobra.NoArgs,
		RunE:  exit(func([]string) (int, error) { return cmdSync(ctx, c, true) }),
	})
	sshSync := keepTerminated(&cobra.Command{
		Use:   "sync",
		Short: "Only the SSH layer (also useful for VS Code Remote-SSH)",
		Args:  cobra.NoArgs,
		RunE:  exit(func([]string) (int, error) { return cmdSync(ctx, c, false) }),
	})
	sshConfig := &cobra.Command{Use: "ssh-config", Short: "Manage only the SSH entries"}
	sshConfig.AddCommand(sshSync)

	attach := &cobra.Command{
		Use:   "attach SESSION",
		Short: "Add one running session (ID or name) as a Herdr machine",
		Args:  oneArg,
		RunE:  exit(func(args []string) (int, error) { return cmdAttach(ctx, c, args[0]) }),
	}
	attach.Flags().StringVar(&c.label, "label", "", "label in the Herdr sidebar (default: the session name)")
	attach.Flags().BoolVar(&c.wait, "wait", false, "wait until the session is running and the box is Herdr-ready (its startup script has installed herdr), e.g. after 'bitrise rde session restore X --wait'")
	attach.Flags().DurationVar(&c.waitTimeout, "wait-timeout", 10*time.Minute, "give up waiting after this long")

	detach := &cobra.Command{
		Use:   "detach SESSION",
		Short: "Remove one session's Herdr machine, SSH entry and host key",
		Args:  oneArg,
		RunE:  exit(func(args []string) (int, error) { return cmdDetach(ctx, c, args[0]) }),
	}
	status := keepTerminated(&cobra.Command{
		Use:   "status",
		Short: "Show session, SSH and Herdr state per session, and flag drift",
		Args:  cobra.NoArgs,
		RunE:  exit(func([]string) (int, error) { return cmdStatus(ctx, c) }),
	})
	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print the plugin version",
		Args:  cobra.NoArgs,
		Run:   func(*cobra.Command, []string) { fmt.Println(version) },
	}
	root.AddCommand(sync, sshConfig, attach, detach, status, versionCmd)
	return root
}

func oneArg(cmd *cobra.Command, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: %s", cmd.UseLine())
	}
	return nil
}

// runCmd captures a command's output.
func runCmd(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	var out, errOut bytes.Buffer
	c := exec.CommandContext(ctx, name, args...)
	c.Stdout, c.Stderr = &out, &errOut
	err := c.Run()
	return out.Bytes(), errOut.Bytes(), err
}

// runInteractive attaches a command to the terminal. Its stdout goes to
// stderr so --format json stays parseable.
func runInteractive(ctx context.Context, name string, args ...string) error {
	c := exec.CommandContext(ctx, name, args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stderr, os.Stderr
	return c.Run()
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func confirm(prompt string) bool {
	fmt.Fprintf(os.Stderr, "%s [y/N] ", prompt)
	var answer string
	fmt.Fscanln(os.Stdin, &answer)
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}
