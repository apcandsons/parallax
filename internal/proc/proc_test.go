package proc

import (
	"net"
	"os"
	"path/filepath"
	"regexp"
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
		procs[i].Stdin = true // config default
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

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestWaitForLogProbe(t *testing.T) {
	m, store := newManager(t,
		config.Process{
			Name:         "dep",
			Run:          `sleep 0.3; echo "listening on :1"; sleep 5`,
			Ready:        &config.Ready{Log: regexp.MustCompile(`listening on`)},
			ReadyTimeout: 5 * time.Second,
		},
		config.Process{Name: "app", Run: `echo started-app`, WaitFor: []string{"dep"}},
	)
	m.StartAll()
	defer m.ForceKillAll()

	if st := m.Procs[1].Status(); !st.Waiting || st.Running {
		t.Fatalf("app should be waiting, got %+v", st)
	}
	if st := m.Procs[0].Status(); !st.Running || st.Ready {
		t.Fatalf("dep should be running but not ready, got %+v", st)
	}
	waitFor(t, "dep ready", func() bool { return m.Procs[0].Status().Ready })
	st := waitExited(t, m, 1)
	if st.ExitCode != 0 || st.WaitFailed {
		t.Fatalf("app = %+v", st)
	}
	lines := store.Proc(1)
	if !findLine(lines, logbuf.Event, "waiting for dep") || !findLine(lines, logbuf.Stdout, "started-app") {
		t.Errorf("app lines = %+v", lines)
	}
	if !findLine(store.Proc(0), logbuf.Event, "ready (log") {
		t.Error("dep should log a ready event")
	}
	if m.Failed() {
		t.Error("nothing failed")
	}
}

func TestWaitForTCPProbe(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln.Close() // free the port; the child re-binds it
	addr := ln.Addr().String()
	_, port, _ := net.SplitHostPort(addr)

	m, _ := newManager(t,
		config.Process{
			Name:         "srv",
			Run:          `sleep 0.3; exec nc -l 127.0.0.1 ` + port,
			Ready:        &config.Ready{TCP: addr},
			ReadyTimeout: 5 * time.Second,
		},
		config.Process{Name: "app", Run: `true`, WaitFor: []string{"srv"}},
	)
	m.StartAll()
	defer m.ForceKillAll()
	waitFor(t, "srv ready", func() bool { return m.Procs[0].Status().Ready })
	if st := waitExited(t, m, 1); st.ExitCode != 0 {
		t.Fatalf("app = %+v", st)
	}
}

func TestWaitForTargetExitsBeforeReady(t *testing.T) {
	m, store := newManager(t,
		config.Process{
			Name:         "dep",
			Run:          `exit 2`,
			Ready:        &config.Ready{Log: regexp.MustCompile(`never`)},
			ReadyTimeout: 5 * time.Second,
		},
		config.Process{Name: "app", Run: `echo nope`, WaitFor: []string{"dep"}},
	)
	m.StartAll()
	st := waitExited(t, m, 1)
	if !st.WaitFailed || st.Running {
		t.Fatalf("app = %+v", st)
	}
	if !findLine(store.Proc(1), logbuf.ErrEvent, "exited before becoming ready") {
		t.Error("expected red not-started line")
	}
	if findLine(store.Proc(1), logbuf.Stdout, "nope") {
		t.Error("app must not have run")
	}
	if !m.Failed() {
		t.Error("wait failure should mark the run failed")
	}
}

func TestReadyTimeoutFailsWaiters(t *testing.T) {
	m, store := newManager(t,
		config.Process{
			Name:         "dep",
			Run:          `sleep 5`,
			Ready:        &config.Ready{Log: regexp.MustCompile(`never`)},
			ReadyTimeout: 200 * time.Millisecond,
		},
		config.Process{Name: "app", Run: `true`, WaitFor: []string{"dep"}},
	)
	m.StartAll()
	defer m.ForceKillAll()
	st := waitExited(t, m, 1)
	if !st.WaitFailed {
		t.Fatalf("app = %+v", st)
	}
	if dst := m.Procs[0].Status(); !dst.Running || !dst.ReadyFailed {
		t.Errorf("dep should keep running with ReadyFailed, got %+v", dst)
	}
	if !findLine(store.Proc(0), logbuf.ErrEvent, "not ready within") {
		t.Error("expected red timeout line on dep")
	}
}

func TestRestartRearmsWaiters(t *testing.T) {
	dir := t.TempDir()
	flag := filepath.Join(dir, "ok")
	m, store := newManager(t,
		config.Process{
			Name:         "dep",
			Run:          `[ -e ` + flag + ` ] && { echo up; sleep 5; } || exit 1`,
			Ready:        &config.Ready{Log: regexp.MustCompile(`^up$`)},
			ReadyTimeout: 5 * time.Second,
		},
		config.Process{Name: "app", Run: `echo ran`, WaitFor: []string{"dep"}},
	)
	m.StartAll()
	defer m.ForceKillAll()
	if st := waitExited(t, m, 1); !st.WaitFailed {
		t.Fatalf("app should have wait-failed first, got %+v", st)
	}

	if err := os.WriteFile(flag, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	m.Restart(0)
	waitFor(t, "app to run after re-arm", func() bool {
		st := m.Procs[1].Status()
		return st.Exited && !st.WaitFailed && st.ExitCode == 0
	})
	if !findLine(store.Proc(1), logbuf.Event, "re-armed") {
		t.Error("expected re-armed event")
	}
}

func TestStopCancelsWait(t *testing.T) {
	m, store := newManager(t,
		config.Process{Name: "dep", Run: `sleep 5`, Ready: &config.Ready{Log: regexp.MustCompile(`x`)}, ReadyTimeout: 5 * time.Second},
		config.Process{Name: "app", Run: `true`, WaitFor: []string{"dep"}},
	)
	m.StartAll()
	defer m.ForceKillAll()
	m.Stop(1)
	st := m.Procs[1].Status()
	if st.Waiting || !st.Exited || st.WaitFailed {
		t.Fatalf("app = %+v", st)
	}
	if !findLine(store.Proc(1), logbuf.Event, "wait cancelled") {
		t.Error("expected wait cancelled event")
	}
	if m.Failed() {
		t.Error("user cancel is not a failure")
	}
}

func TestShutdownReleasesWaiters(t *testing.T) {
	m, _ := newManager(t,
		config.Process{Name: "dep", Run: `sleep 5`, Ready: &config.Ready{Log: regexp.MustCompile(`x`)}, ReadyTimeout: 5 * time.Second},
		config.Process{Name: "app", Run: `true`, WaitFor: []string{"dep"}},
	)
	m.StartAll()
	done := make(chan struct{})
	go func() { m.Shutdown(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown hung")
	}
	if st := m.Procs[1].Status(); st.Running {
		t.Errorf("app must not start during shutdown: %+v", st)
	}
}

func TestStdinForward(t *testing.T) {
	m, store := newManager(t, config.Process{
		Name: "echoer",
		Run:  `while IFS= read -r line; do echo "got:$line"; done`,
	})
	m.StartAll()
	if !m.Write(0, []byte("hel")) || !m.Write(0, []byte("lo\n")) {
		t.Fatal("Write to a running process returned false")
	}
	deadline := time.Now().Add(5 * time.Second)
	for !findLine(store.Proc(0), logbuf.Stdout, "got:hello") {
		if time.Now().After(deadline) {
			t.Fatal("child never echoed the forwarded input")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Order across separate writes must hold too.
	m.Write(0, []byte("a"))
	m.Write(0, []byte("b"))
	m.Write(0, []byte("c\n"))
	for !findLine(store.Proc(0), logbuf.Stdout, "got:abc") {
		if time.Now().After(deadline) {
			t.Fatal("writes were reordered or lost")
		}
		time.Sleep(10 * time.Millisecond)
	}

	m.Stop(0)
	waitExited(t, m, 0)
	if m.Write(0, []byte("x")) {
		t.Error("Write to an exited process should return false")
	}
}

func TestStdinDisabledGetsEOF(t *testing.T) {
	m, store := newManager(t, config.Process{
		Name: "reader",
		Run:  `cat; echo eof`,
	})
	m.Procs[0].Def.Stdin = false
	m.StartAll()
	st := waitExited(t, m, 0) // cat sees EOF immediately from /dev/null
	if st.ExitCode != 0 || !findLine(store.Proc(0), logbuf.Stdout, "eof") {
		t.Errorf("expected clean exit after eof, got %+v", st)
	}
	if m.Write(0, []byte("x")) {
		t.Error("Write with stdin disabled should return false")
	}
}

func TestStdinDropWhenNotRead(t *testing.T) {
	m, store := newManager(t, config.Process{
		Name: "deaf",
		Run:  `exec <&-; sleep 30`, // closes its stdin, never reads
	})
	m.StartAll()
	for i := 0; i < stdinQueue+5; i++ {
		m.Write(0, []byte("x"))
	}
	deadline := time.Now().Add(5 * time.Second)
	for !findLine(store.Proc(0), logbuf.ErrEvent, "not reading input") {
		if time.Now().After(deadline) {
			t.Fatal("expected a drop notice once the queue filled")
		}
		time.Sleep(10 * time.Millisecond)
	}
	n := 0
	for _, l := range store.Proc(0) {
		if strings.Contains(l.Text, "not reading input") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("drop notice logged %d times, want 1", n)
	}
	m.ForceKill(0)
	waitExited(t, m, 0)
}
