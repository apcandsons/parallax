package proc

import (
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/apcandsons/parallax/internal/config"
	"github.com/apcandsons/parallax/internal/logbuf"
)

func newManager(t *testing.T, procs ...config.Process) (*Manager, *logbuf.Store) {
	t.Helper()
	dir := t.TempDir()
	for i := range procs {
		if procs[i].Cwd == "" {
			procs[i].Cwd = dir
		}
		if procs[i].StopSignal == 0 {
			procs[i].StopSignal = syscall.SIGTERM
		}
		if procs[i].StopTimeout == 0 {
			procs[i].StopTimeout = 5 * time.Second
		}
	}
	cfg := &config.Config{
		Settings:  config.Settings{ShutdownTimeout: 5 * time.Second, Scrollback: 1000},
		Processes: procs,
	}
	store := logbuf.NewStore(len(procs), cfg.Settings.Scrollback)
	return NewManager(cfg, store), store
}

func waitExited(t *testing.T, m *Manager, i int) Status {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if st := m.Procs[i].Status(); st.Exited {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d did not exit", i)
	return Status{}
}

func findLine(lines []logbuf.Line, kind logbuf.Kind, substr string) bool {
	for _, l := range lines {
		if l.Kind == kind && strings.Contains(l.Text, substr) {
			return true
		}
	}
	return false
}

func TestCaptureStreamsAndExit(t *testing.T) {
	m, store := newManager(t, config.Process{
		Name: "a",
		Run:  `printf 'out1\n'; printf 'err1\n' >&2; printf 'partial'; exit 3`,
	})
	m.StartAll()
	st := waitExited(t, m, 0)

	if st.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", st.ExitCode)
	}
	if !m.Failed() {
		t.Error("nonzero exit before shutdown should mark the run failed")
	}

	lines := store.Proc(0)
	if !findLine(lines, logbuf.Stdout, "out1") {
		t.Error("stdout line not captured")
	}
	if !findLine(lines, logbuf.Stderr, "err1") {
		t.Error("stderr line not captured as Stderr")
	}
	if !findLine(lines, logbuf.Stdout, "partial") {
		t.Error("partial line without trailing newline not flushed on exit")
	}
	if !findLine(lines, logbuf.ErrEvent, "exited with code 3") {
		t.Error("exit event not logged")
	}
}

func TestMergeOrderAcrossProcs(t *testing.T) {
	m, store := newManager(t,
		config.Process{Name: "a", Run: `echo a1; sleep 0.4; echo a2`},
		config.Process{Name: "b", Run: `sleep 0.2; echo b1`},
	)
	m.StartAll()
	waitExited(t, m, 0)
	waitExited(t, m, 1)

	var got []string
	for _, l := range store.All() {
		if l.Kind == logbuf.Stdout {
			got = append(got, l.Text)
		}
	}
	want := []string{"a1", "b1", "a2"}
	if len(got) != len(want) {
		t.Fatalf("stdout lines = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("merge order = %v, want %v", got, want)
		}
	}
}

func TestGracefulShutdown(t *testing.T) {
	m, _ := newManager(t, config.Process{
		Name: "trapper",
		Run:  `trap 'exit 0' TERM; sleep 30 & wait $!`,
	})
	m.StartAll()
	time.Sleep(200 * time.Millisecond) // let the trap install

	start := time.Now()
	m.Shutdown()
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("graceful shutdown took %v; the trap should make it fast", elapsed)
	}
	st := m.Procs[0].Status()
	if !st.Exited || st.ExitCode != 0 {
		t.Errorf("status = %+v, want clean exit via trap", st)
	}
	if m.Failed() {
		t.Error("shutdown-initiated termination must not count as failure")
	}
}

func TestStubbornProcessGetsKilled(t *testing.T) {
	m, store := newManager(t, config.Process{
		Name:        "stubborn",
		Run:         `trap '' TERM; sleep 30 & wait $!; sleep 30`,
		StopTimeout: 300 * time.Millisecond,
	})
	m.StartAll()
	time.Sleep(200 * time.Millisecond)

	m.Shutdown()
	st := m.Procs[0].Status()
	if !st.Exited || st.Signal == "" {
		t.Errorf("status = %+v, want killed by signal", st)
	}
	if !findLine(store.Proc(0), logbuf.ErrEvent, "SIGKILL") {
		t.Error("SIGKILL escalation not logged")
	}
}

func TestStopThenRestart(t *testing.T) {
	m, store := newManager(t, config.Process{
		Name: "svc",
		Run:  `trap 'exit 0' TERM; sleep 30 & wait $!`,
	})
	m.StartAll()
	time.Sleep(200 * time.Millisecond) // let the trap install

	m.Stop(0)
	st := waitExited(t, m, 0)
	if st.ExitCode != 0 {
		t.Errorf("status = %+v, want clean exit via trap", st)
	}
	if m.Failed() {
		t.Error("user-requested stop must not count as failure")
	}
	if !findLine(store.Proc(0), logbuf.Event, "sending SIGTERM") {
		t.Error("stop signal not logged")
	}

	m.Restart(0)
	deadline := time.Now().Add(5 * time.Second)
	for !m.Procs[0].Status().Running && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !m.Procs[0].Status().Running {
		t.Fatal("process did not restart after a stop")
	}
	m.ForceKill(0)
	st = waitExited(t, m, 0)
	if st.Signal != "SIGKILL" {
		t.Errorf("status = %+v, want killed by SIGKILL", st)
	}
	if m.Failed() {
		t.Error("user-requested kill must not count as failure")
	}
}

func TestRSS(t *testing.T) {
	m, _ := newManager(t,
		config.Process{Name: "sleeper", Run: `sleep 30`},
		config.Process{Name: "done", Run: `true`},
	)
	m.StartAll()
	waitExited(t, m, 1)

	rss := m.RSS()
	if rss[0] <= 0 {
		t.Errorf("running process RSS = %d, want > 0", rss[0])
	}
	if rss[1] != 0 {
		t.Errorf("exited process RSS = %d, want 0", rss[1])
	}
	m.ForceKillAll()
	waitExited(t, m, 0)
}

func TestRestart(t *testing.T) {
	m, store := newManager(t, config.Process{Name: "once", Run: `echo ran`})
	m.StartAll()
	waitExited(t, m, 0)

	m.Restart(0)
	waitExited(t, m, 0)

	count := 0
	for _, l := range store.Proc(0) {
		if l.Kind == logbuf.Stdout && l.Text == "ran" {
			count++
		}
	}
	if count != 2 {
		t.Errorf("output appeared %d times, want 2 (original + restart)", count)
	}
}
