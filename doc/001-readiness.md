# parallax — Readiness-gated start (issue #1)

Adds two optional per-process fields to `.parallax.yaml`:

```yaml
iam:
  run: make -C ../gve-iam dev-pg
  ready: { tcp: 127.0.0.1:17001 }     # or { http: http://127.0.0.1:17201/healthz } / { log: "listening on" }
dvp-api:
  run: make -C ../gve-dvp run ENV=local-integration
  wait_for: [iam, 3wdb]
```

## Semantics

- `wait_for` delays the **start** of a process until every named process has passed its `ready` probe. A process without `ready` counts as ready once started (plain `depends_on`).
- Probes: `tcp` (connect succeeds), `http` (2xx response), `log` (regex matched against the process's own output). Polled every 500ms.
- `ready_timeout` (default `60s`, per-process override) bounds the probe. Timeout marks the process failed with a red line.
- A `wait_for` target that exits or times out before becoming ready fails the waiter's start with a red line instead of hanging. `r` on the target re-arms its waiters.
- Cycles and references to unknown processes are config errors at load time.
- The TUI shows a running-but-not-ready slot distinctly (e.g. `~` marker) and a `waiting` state for gated processes.
- Configs without these fields behave exactly as before.

Supersedes the `depends_on` line under "Out of scope for v1" in `000-design.md`.
