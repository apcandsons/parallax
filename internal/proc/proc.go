// Package proc spawns the configured processes, reads their pipes into the
// log store, and delivers signals. It has no knowledge of the TUI.
package proc

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/apcandsons/parallax/internal/config"
	"github.com/apcandsons/parallax/internal/logbuf"
)

type Status struct {
	Waiting  bool // start is gated on wait_for targets that aren't ready yet
	Running  bool
	Exited   bool
	ExitCode int    // -1 when terminated by a signal (or never started)
	Signal   string // signal description when terminated by one

	// Ready is set once the run passed its ready probe (immediately on start
	// for processes without one). It stays set after exit, so a one-shot
	// task that finished cleanly still counts as ready for its waiters.
	Ready bool
	// ReadyFailed is set when the probe timed out; the process keeps running
	// but its waiters are failed.
	ReadyFailed bool
	// WaitFailed is set when a wait_for target exited or failed its probe
	// before becoming ready; the process was never started. Restarting the
	// target re-arms it.
	WaitFailed bool
}

type Proc struct {
	Def   config.Process
	Index int

	mu      sync.Mutex
	cmd     *exec.Cmd
	st      Status
	stopped bool          // a user-requested stop signal was sent to this run
	done    chan struct{} // closed when the current run is reaped
	run     uint64        // incremented per spawn; readiness events for old runs are ignored
	waitGen uint64        // incremented per wait; cancels a superseded/aborted wait
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

	changeMu sync.Mutex
	change   chan struct{} // closed and replaced on every status change; waiters block on it
	byName   map[string]*Proc
}

func NewManager(cfg *config.Config, store *logbuf.Store) *Manager {
	m := &Manager{store: store, change: make(chan struct{}), byName: map[string]*Proc{}}
	for i, def := range cfg.Processes {
		p := &Proc{Def: def, Index: i}
		m.Procs = append(m.Procs, p)
		m.byName[def.Name] = p
	}
	return m
}

// changed wakes every goroutine blocked on changeCh.
func (m *Manager) changed() {
	m.changeMu.Lock()
	close(m.change)
	m.change = make(chan struct{})
	m.changeMu.Unlock()
}

func (m *Manager) changeCh() <-chan struct{} {
	m.changeMu.Lock()
	defer m.changeMu.Unlock()
	return m.change
}

func (m *Manager) StartAll() {
	for _, p := range m.Procs {
		m.start(p)
	}
}

// start spawns p, or, if it has wait_for targets, parks it in the waiting
// state until they are ready. Returns immediately either way.
func (m *Manager) start(p *Proc) {
	if len(p.Def.WaitFor) == 0 {
		m.spawn(p)
		return
	}
	p.mu.Lock()
	if p.st.Running || p.st.Waiting {
		p.mu.Unlock()
		return
	}
	p.waitGen++
	gen := p.waitGen
	p.st = Status{Waiting: true}
	p.mu.Unlock()
	m.changed()

	m.store.Append(p.Index, logbuf.Event, "waiting for "+strings.Join(p.Def.WaitFor, ", "))
	go m.waitThenSpawn(p, gen)
}

func (m *Manager) waitThenSpawn(p *Proc, gen uint64) {
	for {
		if m.shuttingDown.Load() {
			return
		}
		ch := m.changeCh() // grab before checking, so a change during the check isn't missed

		p.mu.Lock()
		cancelled := p.waitGen != gen
		p.mu.Unlock()
		if cancelled {
			return
		}

		pending, failed := m.depState(p)
		if failed != "" {
			p.mu.Lock()
			p.st = Status{Exited: true, ExitCode: -1, WaitFailed: true}
			p.mu.Unlock()
			m.failed.Store(true)
			m.store.Append(p.Index, logbuf.ErrEvent, "not started: "+failed)
			m.changed()
			return
		}
		if pending == "" {
			p.mu.Lock()
			p.st = Status{}
			p.mu.Unlock()
			m.spawn(p)
			return
		}
		<-ch
	}
}

// depState reports the first wait_for target that isn't ready yet (pending)
// and, if any target can no longer become ready, why (failed).
func (m *Manager) depState(p *Proc) (pending, failed string) {
	for _, name := range p.Def.WaitFor {
		st := m.byName[name].Status()
		switch {
		case st.Ready:
		case st.ReadyFailed:
			return "", name + " failed its ready probe"
		case st.Exited:
			if st.WaitFailed {
				return "", name + " was not started"
			}
			return "", name + " exited before becoming ready"
		default:
			if pending == "" {
				pending = name
			}
		}
	}
	return pending, ""
}

// rearm restarts every process whose wait on target had failed. Called after
// the user restarts target.
func (m *Manager) rearm(target *Proc) {
	for _, q := range m.Procs {
		if slices.Contains(q.Def.WaitFor, target.Def.Name) && q.Status().WaitFailed {
			m.store.Append(q.Index, logbuf.Event, "re-armed by restart of "+target.Def.Name)
			m.start(q)
		}
	}
}

// Failed reports whether any process ended abnormally before shutdown was
// initiated. Deaths caused by our own stop signals don't count.
func (m *Manager) Failed() bool { return m.failed.Load() }

func (m *Manager) spawn(p *Proc) {
	p.mu.Lock()
	if p.st.Running || p.st.Waiting {
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
		m.changed()
		return
	}

	done := make(chan struct{})
	p.cmd = cmd
	p.st = Status{Running: true, Ready: p.Def.Ready == nil}
	p.stopped = false
	p.done = done
	p.run++
	run := p.run
	p.mu.Unlock()
	m.changed()

	m.store.Append(p.Index, logbuf.Event, fmt.Sprintf("started (pid %d)", cmd.Process.Pid))

	var readers sync.WaitGroup
	readers.Add(2)
	go m.readPipe(p, run, stdout, logbuf.Stdout, &readers)
	go m.readPipe(p, run, stderr, logbuf.Stderr, &readers)
	if p.Def.Ready != nil {
		go m.probe(p, run, done)
	}

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
		st.Ready = p.st.Ready
		st.ReadyFailed = p.st.ReadyFailed
		p.st = st
		p.cmd = nil
		userStopped := p.stopped
		p.mu.Unlock()

		if !m.shuttingDown.Load() && !userStopped && st.ExitCode != 0 {
			m.failed.Store(true)
		}
		m.store.Append(p.Index, logbuf.ErrEvent, msg)
		close(done)
		m.changed()
	}()
}

// markReady records that run passed its probe. Stale runs are ignored.
func (m *Manager) markReady(p *Proc, run uint64) {
	p.mu.Lock()
	if p.run != run || p.st.Ready || p.st.ReadyFailed {
		p.mu.Unlock()
		return
	}
	p.st.Ready = true
	p.mu.Unlock()
	m.store.Append(p.Index, logbuf.Event, "ready ("+p.Def.Ready.String()+")")
	m.changed()
}

func (m *Manager) probeDone(p *Proc, run uint64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.run != run || p.st.Ready || p.st.ReadyFailed || !p.st.Running
}

// probe polls a tcp/http readiness check (log probes are fed by readPipe
// instead) until the run is ready, exits, or the ready timeout elapses.
func (m *Manager) probe(p *Proc, run uint64, done <-chan struct{}) {
	r := p.Def.Ready
	timeout := time.After(p.Def.ReadyTimeout)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		if m.probeDone(p, run) {
			return
		}
		select {
		case <-done:
			return
		case <-timeout:
			p.mu.Lock()
			if p.run != run || p.st.Ready || !p.st.Running {
				p.mu.Unlock()
				return
			}
			p.st.ReadyFailed = true
			p.mu.Unlock()
			m.failed.Store(true)
			m.store.Append(p.Index, logbuf.ErrEvent,
				fmt.Sprintf("not ready within %s (%s)", p.Def.ReadyTimeout, r.String()))
			m.changed()
			return
		case <-ticker.C:
			if r.Log == nil && checkProbe(r) {
				m.markReady(p, run)
				return
			}
		}
	}
}

var probeClient = &http.Client{Timeout: 2 * time.Second}

func checkProbe(r *config.Ready) bool {
	switch {
	case r.TCP != "":
		c, err := net.DialTimeout("tcp", r.TCP, 2*time.Second)
		if err != nil {
			return false
		}
		c.Close()
		return true
	case r.HTTP != "":
		resp, err := probeClient.Get(r.HTTP)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode >= 200 && resp.StatusCode < 300
	}
	return false
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
	if st.Waiting {
		m.store.Append(i, logbuf.Event, "still waiting; not restarted")
		return
	}
	m.store.Append(i, logbuf.Event, "restarting")
	m.start(p)
	m.rearm(p)
}

// Stop sends one running process its configured stop signal. Exited processes
// are left alone; the caller escalates with ForceKill if it doesn't die.
func (m *Manager) Stop(i int) {
	if m.shuttingDown.Load() {
		return
	}
	p := m.Procs[i]
	if m.cancelWait(p) {
		return
	}
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

// cancelWait aborts a pending wait_for wait. Reports whether p was waiting.
func (m *Manager) cancelWait(p *Proc) bool {
	p.mu.Lock()
	if !p.st.Waiting {
		p.mu.Unlock()
		return false
	}
	p.waitGen++
	p.st = Status{Exited: true, ExitCode: -1}
	p.mu.Unlock()
	m.store.Append(p.Index, logbuf.Event, "wait cancelled")
	m.changed()
	return true
}

// ForceKill SIGKILLs one running process group.
func (m *Manager) ForceKill(i int) {
	p := m.Procs[i]
	if m.cancelWait(p) {
		return
	}
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
	m.changed() // release waiters
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
	m.changed()
	for _, p := range m.Procs {
		p.signalGroup(syscall.SIGKILL)
	}
}

func (m *Manager) readPipe(p *Proc, run uint64, r io.Reader, kind logbuf.Kind, wg *sync.WaitGroup) {
	defer wg.Done()
	var logRE *regexp.Regexp
	if p.Def.Ready != nil {
		logRE = p.Def.Ready.Log
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		text := sanitize(sc.Text())
		m.store.Append(p.Index, kind, text)
		if logRE != nil && logRE.MatchString(text) {
			m.markReady(p, run)
			logRE = nil
		}
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
