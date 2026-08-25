# parallax

`parallax` runs multiple long-lived processes from one terminal and multiplexes their logs into a TUI. It's for local development setups where you'd otherwise open four terminal tabs and lose track of which one printed the error.

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

Every line gets a millisecond timestamp stamped at arrival, so when two services race, the merged view shows exactly which line landed first. Nothing runs in the background; quitting the TUI stops everything.

## Install

```sh
make install          # builds to ~/.local/bin/parallax
```

Or build locally:

```sh
make build            # builds to bin/parallax
```

Requires Go 1.22+.

## Usage

```
parallax              # reads .parallax.yaml in the current directory
parallax -f path.yaml # explicit config path
parallax webui sor    # run only the named processes
```

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
| `run` | required | command, executed via `/bin/sh -c` |
| `cwd` | config file's directory | working directory |
| `env` | inherited | extra environment variables (map) |
| `color` | auto-assigned | override the label color |
| `stop_signal` | `SIGTERM` | signal sent on shutdown |
| `stop_timeout` | `settings.shutdown_timeout` | per-process grace period |
| `ready` | none | readiness probe: `{ tcp: host:port }`, `{ http: url }` (2xx), or `{ log: regexp }` (matched against the process's own output) |
| `ready_timeout` | `60s` | how long the probe may take; on timeout the process is marked failed (red line) and its waiters fail |
| `wait_for` | none | list of process names; start is delayed until each is ready (a process without `ready` is ready once started) |

Process order in the file determines display order and color assignment, so the layout is stable across runs.

Readiness gating, for a stack where one process needs another's port up before it boots:

```yaml
iam:
  run: make -C ../gve-iam dev-pg
  ready: { tcp: 127.0.0.1:17001 }
dvp-api:
  run: make -C ../gve-dvp run ENV=local-integration
  wait_for: [iam]
```

A `wait_for` target that exits (or times out) before becoming ready fails the waiter with a red line instead of hanging; `r` on the target re-arms its waiters. Cycles and unknown names are config errors. When running a subset (`parallax api`), waits on processes outside the subset are dropped. See [doc/001-readiness.md](doc/001-readiness.md).

There's a runnable example in [`examples/demo/.parallax.yaml`](examples/demo/.parallax.yaml):

```sh
cd examples/demo && parallax
```

## Keybindings

| key | action |
|---|---|
| `0`–`9` | select slot directly (0 is the merged `all` view) |
| `tab` / `shift-tab`, `←`/`→` | cycle selection |
| `↑`/`↓`, `pgup`/`pgdn` | scroll; any scroll pauses follow mode |
| `f` or `end` | resume follow (tail) mode |
| `r` | restart the selected process |
| `ctrl-x` | stop the selected process; press again to SIGKILL |
| `ctrl-c` or `q` | graceful shutdown of everything |

The selector bar shows liveness: running processes render in their assigned color with current memory use (RSS summed over the process group), exited ones in red with the exit code, e.g. `[2-sor ✗1]`. A process parked on `wait_for` shows `⧗`, one running but not yet past its `ready` probe shows `~`, a failed probe `✗ready`, and a waiter whose target never became ready `✗wait`.

## Behavior notes

Each process runs in its own process group, so stop signals reach the whole tree, including whatever `make` spawns. When a process exits, the others keep running; `r` restarts it. On shutdown, parallax sends each process its stop signal, waits out the grace period while still streaming logs, then SIGKILLs anything left. A second `ctrl-c` skips the wait.

ANSI escape codes in child output are stripped. stderr renders the same as stdout.

The full design, including rationale and what's deliberately out of scope for v1 (automatic restarts, log persistence, search), is in [doc/000-design.md](doc/000-design.md).

## Development

```sh
make test
make vet
```
