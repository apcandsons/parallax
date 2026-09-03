# 002: stdin forwarding and the ctrl-a prefix

## Problem

Some dev processes take keyboard input: a load simulator that starts a burst on `b`, a REPL, a server that reloads on `enter`. Under parallax they got `/dev/null` and the keys were unreachable, so the tool had to be run in its own tab, which is the situation parallax exists to avoid.

Giving processes stdin creates a second problem: parallax's own keys (`q`, `r`, `1`..`9`, `ctrl-x`) are exactly the kind of keys a child wants.

## Design

**Every process gets a stdin pipe.** With a process selected, every key event is written to that pipe. `enter` is sent as `\n`, since the child reads a pipe and there is no tty line discipline to translate `\r` for it; control keys are sent as their byte (`ctrl-c` is `0x03`, not a signal); pasted text is written as one chunk; alt-modified keys get an `ESC` prefix. Keys with no byte form (arrows, function keys) are not forwarded, and the scroll keys keep scrolling the pane without a prefix, since a non-tty child can't use them anyway.

**`ctrl-a` is the command prefix**, as in `screen`. `ctrl-a x` stops (again to kill), `ctrl-a r` restarts, `ctrl-a q` shuts everything down, `ctrl-a 0`..`9` selects, `ctrl-a a` sends a literal `ctrl-a`, `ctrl-a esc` cancels. The prefix has no timeout; the bar shows the command list while it is armed. In the `all` view there is no stdin target, so the pre-002 unprefixed keys keep working there. Two exceptions hold in every view: `ctrl-a` is never forwarded (use `ctrl-a a`), and once a shutdown is under way a bare `ctrl-c` forces it, so the second-ctrl-c habit still works.

**Writes never block the UI.** Each run has a writer goroutine fed by a bounded queue (256 events). A child that never reads stdin fills the pipe and then the queue; further input is dropped and one red line per run says so. The pipe is closed when the run is reaped, so a restarted process gets a fresh one and the old run's leftovers don't leak into it.

**Opt out per process** with `stdin: false`, which restores `/dev/null`. This matters for a `run` that reads stdin to EOF (`cat`, `xargs`, some build scripts), which would otherwise wait forever.

## What this is not

Not a pty. Children see a pipe, so they don't enter raw mode, don't get line editing, and don't receive window-size changes. That keeps stdout/stderr capture, ANSI stripping, and the merged log view exactly as they are; a pty would merge the two streams and hand over cursor control. A `tty: true` mode that trades log fidelity for a real terminal could be layered on later if a process turns out to need it.
