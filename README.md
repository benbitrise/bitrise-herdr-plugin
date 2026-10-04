# bitrise :herdr

🚨 This is vibe coded. Use at your own risk

A [Bitrise CLI](https://github.com/bitrise-io/bitrise) plugin that keeps your laptop's
[Herdr](https://herdr.dev) machine list and SSH config in step with your running Bitrise RDE
sessions. One command takes every running session to "visible and usable in the Herdr
sidebar", and the same command cleans up after sessions that are gone.

```sh
bitrise plugin install https://github.com/benbitrise/bitrise-herdr-plugin.git   # or: bitrise plugin install ./
bitrise :herdr sync
```

Needs Bitrise CLI 3.1.0+ (for `bitrise rde`), herdr 0.9+, OpenSSH, and Go to build from source.

## Commands

| Command | Does |
|---|---|
| `bitrise :herdr sync` | Writes SSH entries and adds Herdr machines for every running session; removes both for terminated or deleted sessions. Safe to run before every `herdr` launch. |
| `bitrise :herdr ssh-config sync` | The SSH layer only. Also useful for VS Code Remote-SSH and Zed. |
| `bitrise :herdr attach SESSION [--label X] [--wait]` | One running session, by ID, name or alias. `--wait` first waits (up to `--wait-timeout`, default 10m) until the session is running and passes the Herdr readiness check. Exits non-zero if the session wasn't attached. |
| `bitrise :herdr detach SESSION` | Removes one session's Herdr machine, SSH entry and host key. Works after the session is gone. |
| `bitrise :herdr status` | Session, SSH and Herdr state per session, with pending drift. Read-only. |

The workspace resolves like other `bitrise rde` commands: `--workspace`, then
`BITRISE_WORKSPACE_ID`, then `bitrise config set default_workspace_id <id>`.

## What it changes on your laptop

- **`~/.ssh/config.d/rde-<name>`**, one file per running session, mode 600. Each starts with
  `# managed by bitrise :herdr; session=<id>`. Files without that line are never modified or
  deleted. Sessions that share a name get a short ID suffix (`rde-<name>-<id>`), and a session
  keeps its alias once it has one, so a same-named session never renames yours.
- **`~/.ssh/config`** gets `Include ~/.ssh/config.d/rde-*` above its first `Host` block, if it
  isn't there already. An Include after a Host block is silently scoped to that block. The
  original is backed up once to `~/.ssh/config.bak-bitrise-herdr`. Every sync puts the line
  back if it's gone. A symlinked config is edited through the link; if the target is read-only
  (home-manager, for example), sync prints the line to add yourself.
- **`~/.ssh/known_hosts_rde`** holds RDE host keys (`StrictHostKeyChecking accept-new`), never
  your main `known_hosts`. Keys are removed on detach, and before writing an entry, because RDE
  reuses hostnames.
- **Herdr machines** named `rde-<name>`, labelled with the session name, using the box's
  `default` Herdr session. The plugin records the profile IDs it creates in its data dir
  (`~/.bitrise/plugins/herdr/data/state.json`) and only ever removes those.

It never creates, terminates or deletes RDE sessions.

## How sync decides

The RDE API is the source of truth.

1. List sessions (`bitrise rde session list`) and fetch SSH details for running ones
   (`session view`). **If listing fails, sync exits non-zero and changes nothing.**
2. **Running** sessions get an SSH entry (rewritten if the address changed) and, if none
   targets the alias, a Herdr machine.
3. **Terminated** sessions lose their machine, SSH entry and host key, unless you pass
   `--keep-terminated`. Sessions that are **gone** from the list are cleaned up the same way.
4. Sessions that are **starting, restoring or otherwise in between**, or whose details couldn't
   be fetched, are left exactly as they are.

## Template prerequisites

Sessions should come from a template that makes them Herdr-ready. The plugin checks these and
names the fix, but never changes the box:

- ☑️ Your SSH public key as a saved input, so the box accepts it without a password.
- ☑️ herdr compatible with your laptop's (same endpoint protocol generation and the capabilities
  `herdr machine add` needs; the same release is simplest), with `herdr integration install claude` run.
- ☑️ `herdr` and `claude` on PATH. A login shell is required; the non-interactive SSH PATH
  (`~/.zshenv` on macOS) is recommended, and missing it only gives a warning.

## Development

```sh
go test ./...
```

Herdr is AGPL-3.0, so it's only ever called as an external binary.
