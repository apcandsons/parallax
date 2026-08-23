# parallax — Design

`parallax` runs multiple long-lived processes from one terminal and multiplexes their logs into a TUI. It's for local development setups where you'd otherwise open four terminal tabs and lose track of which one printed the error.

## Invocation

```
parallax              # reads .parallax.yaml in the current directory
parallax -f path.yaml # explicit config path
parallax webui sor    # run only the named processes
```

If the config file is missing or has no process definitions, print an error and exit 1. Nothing runs in the background; quitting the TUI stops everything.

## Configuration

`.parallax.yaml` is a flat map of process name → definition, plus an optional reserved `settings` key:

```yaml
settings:
  shutdown_timeout: 10s   # grace period before SIGKILL (default 10s)
  scrollback: 10000       # lines kept per process buffer (default 10000)

webui:
  run: make -C ./webui run
sor:
  run: make -C ./server run
kms-emu:
  run: make -C ./emulators/kms run
```

Per-process fields:

| field | default | meaning |
|---|---|---|
| `run` | required | command, executed via `/bin/sh -c` (predictable POSIX syntax regardless of the user's login shell) |
| `cwd` | config file's directory | working directory |
| `env` | inherited | extra environment variables (map) |
| `color` | auto-assigned | override the label color |
| `stop_signal` | `SIGTERM` | signal sent on shutdown |
| `stop_timeout` | `settings.shutdown_timeout` | per-process grace period |

`settings` is reserved; a process can't use that name. Process order in the file determines display order and color assignment, so the layout is stable across runs.

## TUI layout

Two regions: a log pane on top, a process selector bar at the bottom.

```
┌──────────────────────────────────────────────────────────────┐
│ [2026-08-11 12:34:56.010] webui:   Server started on :3000   │
│ [2026-08-11 12:34:56.011] sor:     System of record started  │
│ [2026-08-11 12:34:56.012] kms-emu: Accepting on 0.0.0.0:18027│
│ ...                                                          │
├──────────────────────────────────────────────────────────────┤
│ [0-all] [1-webui] [2-sor] [3-kms-emu]                        │
└──────────────────────────────────────────────────────────────┘
```

Slot 0 is always `all`: every process's lines merged in arrival order. Slots 1..n are the individual processes in config order. Selecting a process filters the pane to just its lines; the buffers are shared, so switching views is instant and lossless.

The selector shows liveness: a running process renders its label in its assigned color, an exited one in red with the exit code, e.g. `[2-sor ✗1]`. Running slots also show memory use, e.g. `[2-sor:25MB]`, and slot 0 shows the total: RSS summed over each run's process group, sampled via `ps` every 2 seconds.

### Keybindings

| key | action |
|---|---|
| `0`–`9` | select slot directly |
| `tab` / `shift-tab`, `←`/`→` | cycle selection |
| `↑`/`↓`, `pgup`/`pgdn` | scroll; any scroll pauses follow mode |
| `f` or `end` | resume follow (tail) mode |
| `r` | restart the selected process |
| `ctrl-x` | stop the selected process (its stop signal); press again to SIGKILL |
| `ctrl-c` | graceful shutdown of everything |

## Log format

Every line is prefixed with a wall-clock timestamp at millisecond precision and the process name:

```
[2026-08-11 12:34:56.010] webui:   Server started on 0.0.0.0:3000
[2026-08-11 12:34:56.011] sor:     System of record started: default.conf
[2026-08-11 12:34:56.012] kms-emu: Accepting connection on 0.0.0.0:18027
```

Coloring:

- **Process name** — each process gets a color from a fixed palette (cyan, green, magenta, yellow, blue, ...), cycling by config order. The same name keeps the same color for the whole run, in both the log pane and the selector bar.
- **stdout** — terminal default.
- **stderr** — terminal default, same as stdout. (v1 rendered stderr in amber so warnings would stand out, but most tools log all routine output to stderr, which turned entire panes amber.)
- **Lifecycle events** — red. When a process exits, parallax injects a line into its stream: `[ts] sor: exited with code 1 (signal: none)`. Start and restart events get a dim gray line the same way.
- **Timestamp** — dim, so it doesn't compete with content.

The millisecond timestamps are the point of the merged view: when two services race, the `all` stream shows exactly which line landed first. Timestamps are stamped by parallax at read time, not by the child, so they reflect when output arrived, not when it was written (pipe buffering in the child can skew this; that's inherent to log capture and worth documenting, not fixing).

ANSI escape codes in child output are stripped in v1. Passing them through fights with our own coloring; revisit later with a "raw" toggle if it hurts.

## Process management

Each process runs in its own process group (`setpgid`), so signals reach the whole tree, including whatever `make` spawns. stdout and stderr are captured through separate pipes, read line-buffered. A partial line (no trailing newline) is flushed either when the newline eventually arrives or when the process exits.

Per-process ring buffer holds the last `scrollback` lines. The `all` view is a merge over the per-process buffers by arrival sequence number, not a separate copy.

### Exit

When a process exits, parallax logs the red exit line and marks the slot. Other processes keep running. `r` restarts it manually; automatic restart policies (`restart: on-failure`) are out of scope for v1 but the config field is reserved.

### Shutdown

`ctrl-c` (or `q`) starts the shutdown sequence:

1. Send `stop_signal` (default SIGTERM) to every process group.
2. Wait up to `shutdown_timeout` (default 10s, configurable globally and per process).
3. SIGKILL anything still alive.
4. Exit once all children are reaped. Exit code is 0 if everything terminated by our signal, 1 if any process had already failed.

During the grace period the TUI stays up and keeps streaming logs, with a status line like `shutting down... 7s`. A second `ctrl-c` skips the wait and goes straight to SIGKILL.

## Implementation notes

Go, with [bubbletea](https://github.com/charmbracelet/bubbletea) + lipgloss for the TUI. Single static binary, good process-group and signal ergonomics, and the log-multiplexing model (goroutine per pipe feeding a central channel that assigns sequence numbers) maps directly onto the design. Rust/ratatui is the alternative if the project ever needs it; nothing here depends on the choice.

Internal structure:

- `config` — YAML parsing and validation.
- `proc` — spawn, pipe reading, signal delivery, reaping. No TUI knowledge.
- `logbuf` — per-process ring buffers, sequence numbering, merged iteration.
- `ui` — bubbletea model: log viewport, selector bar, keybindings.

`proc` and `logbuf` are testable without a terminal; the racing-condition timestamp behavior in particular gets tested by spawning two scripted children and asserting merge order.

## Out of scope for v1

- Automatic restarts and health checks
- Dependency ordering between processes (`depends_on`)
- Log persistence to disk
- Search within the log pane
- Attaching stdin to a child process
