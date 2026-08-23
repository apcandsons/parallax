// Package proc spawns the configured processes, reads their pipes into the
// log store, and delivers signals. It has no knowledge of the TUI.
package proc

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/apcandsons/parallax/internal/config"
	"github.com/apcandsons/parallax/internal/logbuf"
)

type Status struct {
	Running  bool
	Exited   bool
	ExitCode int    // -1 when terminated by a signal (or never started)
	Signal   string // signal description when terminated by one
}

type Proc struct {
	Def   config.Process
	Index int

	mu      sync.Mutex
	cmd     *exec.Cmd
	st      Status
	stopped bool          // a user-requested stop signal was sent to this run
	done    chan struct{} // closed when the current run is reaped
}

func (p *Proc) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.st
}

func (p *Proc) doneCh() chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.done
}

// Pgid returns the process-group id of the current run (equal to its pid,
// thanks to setpgid), or 0 when not running.
func (p *Proc) Pgid() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// signalGroup signals the whole process group (setpgid puts each run in its
// own group, so this reaches whatever the shell command spawned).
func (p *Proc) signalGroup(sig syscall.Signal) {
	p.mu.Lock()
	cmd := p.cmd
	p.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, sig)
}

type Manager struct {
	Procs []*Proc

	store        *logbuf.Store
	shuttingDown atomic.Bool
	failed       atomic.Bool
}

func NewManager(cfg *config.Config, store *logbuf.Store) *Manager {
	m := &Manager{store: store}
	for i, def := range cfg.Processes {
		m.Procs = append(m.Procs, &Proc{Def: def, Index: i})
	}
	return m
}

func (m *Manager) StartAll() {
	for _, p := range m.Procs {
		m.start(p)
	}
}

// Failed reports whether any process ended abnormally before shutdown was
// initiated. Deaths caused by our own stop signals don't count.
func (m *Manager) Failed() bool { return m.failed.Load() }

func (m *Manager) start(p *Proc) {
	p.mu.Lock()
	if p.st.Running {
		p.mu.Unlock()
		return
	}

	cmd := exec.Command("/bin/sh", "-c", p.Def.Run)
	cmd.Dir = p.Def.Cwd
	env := os.Environ()
	for k, v := range p.Def.Env {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, outErr := cmd.StdoutPipe()
	stderr, errErr := cmd.StderrPipe()
	var startErr error
	if outErr != nil {
		startErr = outErr
	} else if errErr != nil {
		startErr = errErr
	} else {
		startErr = cmd.Start()
	}
	if startErr != nil {
		p.st = Status{Exited: true, ExitCode: -1}
		p.mu.Unlock()
		m.failed.Store(true)
		m.store.Append(p.Index, logbuf.ErrEvent, "failed to start: "+startErr.Error())
		return
	}

	done := make(chan struct{})
	p.cmd = cmd
	p.st = Status{Running: true}
	p.stopped = false
	p.done = done
	p.mu.Unlock()

	m.store.Append(p.Index, logbuf.Event, fmt.Sprintf("started (pid %d)", cmd.Process.Pid))

	var readers sync.WaitGroup
	readers.Add(2)
	go m.readPipe(p, stdout, logbuf.Stdout, &readers)
	go m.readPipe(p, stderr, logbuf.Stderr, &readers)

	go func() {
		// cmd.Wait closes the pipes, so it must not run until both readers
		// hit EOF.
		readers.Wait()
		_ = cmd.Wait()

		st := Status{Exited: true, ExitCode: -1}
		msg := "exited"
		if ps := cmd.ProcessState; ps != nil {
			if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				st.Signal = config.SignalName(ws.Signal())
				msg = "terminated by " + st.Signal
			} else {
				st.ExitCode = ps.ExitCode()
				msg = fmt.Sprintf("exited with code %d", st.ExitCode)
			}
		}

		p.mu.Lock()
		p.st = st
		p.cmd = nil
		userStopped := p.stopped
		p.mu.Unlock()

		if !m.shuttingDown.Load() && !userStopped && st.ExitCode != 0 {
			m.failed.Store(true)
		}
		m.store.Append(p.Index, logbuf.ErrEvent, msg)
		close(done)
	}()
}

// Restart respawns an exited process. Running processes are left alone.
func (m *Manager) Restart(i int) {
	if m.shuttingDown.Load() {
		return
	}
	p := m.Procs[i]
	st := p.Status()
	if st.Running {
		m.store.Append(i, logbuf.Event, "still running; not restarted")
		return
	}
	m.store.Append(i, logbuf.Event, "restarting")
	m.start(p)
}

// Stop sends one running process its configured stop signal. Exited processes
// are left alone; the caller escalates with ForceKill if it doesn't die.
func (m *Manager) Stop(i int) {
	if m.shuttingDown.Load() {
		return
	}
	p := m.Procs[i]
	p.mu.Lock()
	running := p.st.Running
	p.stopped = p.stopped || running
	p.mu.Unlock()
	if !running {
		return
	}
	m.store.Append(i, logbuf.Event, "sending "+config.SignalName(p.Def.StopSignal))
	p.signalGroup(p.Def.StopSignal)
}

// ForceKill SIGKILLs one running process group.
func (m *Manager) ForceKill(i int) {
	p := m.Procs[i]
	p.mu.Lock()
	running := p.st.Running
	p.stopped = p.stopped || running
	p.mu.Unlock()
	if !running {
		return
	}
	m.store.Append(i, logbuf.ErrEvent, "sending SIGKILL")
	p.signalGroup(syscall.SIGKILL)
}

// Shutdown sends each process its stop signal, waits up to its stop timeout,
// SIGKILLs stragglers, and returns once everything is reaped. Idempotent;
// concurrent calls beyond the first return immediately.
func (m *Manager) Shutdown() {
	if !m.shuttingDown.CompareAndSwap(false, true) {
		return
	}
	var wg sync.WaitGroup
	for _, p := range m.Procs {
		if !p.Status().Running {
			continue
		}
		wg.Add(1)
		go func(p *Proc) {
			defer wg.Done()
			done := p.doneCh()
			m.store.Append(p.Index, logbuf.Event, "sending "+config.SignalName(p.Def.StopSignal))
			p.signalGroup(p.Def.StopSignal)
			select {
			case <-done:
			case <-time.After(p.Def.StopTimeout):
				m.store.Append(p.Index, logbuf.ErrEvent,
					fmt.Sprintf("did not stop within %s; sending SIGKILL", p.Def.StopTimeout))
				p.signalGroup(syscall.SIGKILL)
				<-done
			}
		}(p)
	}
	wg.Wait()
}

// ForceKillAll SIGKILLs every running process group immediately. Used for the
// second ctrl-c and as a safety net when the UI exits abnormally.
func (m *Manager) ForceKillAll() {
	m.shuttingDown.Store(true)
	for _, p := range m.Procs {
		p.signalGroup(syscall.SIGKILL)
	}
}

func (m *Manager) readPipe(p *Proc, r io.Reader, kind logbuf.Kind, wg *sync.WaitGroup) {
	defer wg.Done()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		m.store.Append(p.Index, kind, sanitize(sc.Text()))
	}
	// A partial final line (no trailing newline) is returned by Scan before
	// EOF, so it is flushed by the loop above. Errors here are read errors,
	// e.g. a single line over 1MB.
	if err := sc.Err(); err != nil {
		m.store.Append(p.Index, logbuf.ErrEvent, "log read error: "+err.Error())
	}
}

// ansiRE matches CSI sequences, OSC sequences, and lone two-byte escapes.
var ansiRE = regexp.MustCompile(`\x1b(?:\[[0-9;?]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[@-Z\\-_])`)

func sanitize(s string) string {
	s = ansiRE.ReplaceAllString(s, "")
	// Progress bars redraw with \r; keep only the final state of the line.
	if i := strings.LastIndexByte(s, '\r'); i >= 0 {
		s = s[i+1:]
	}
	return strings.ReplaceAll(s, "\t", "    ")
}
