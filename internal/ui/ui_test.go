package ui

import (
	"bytes"
	"strings"
	"syscall"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/apcandsons/parallax/internal/config"
	"github.com/apcandsons/parallax/internal/logbuf"
	"github.com/apcandsons/parallax/internal/proc"
)

func TestKeyBytes(t *testing.T) {
	cases := []struct {
		msg  tea.KeyMsg
		want []byte
	}{
		{tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b")}, []byte("b")},
		{tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("pasted text"), Paste: true}, []byte("pasted text")},
		{tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("é")}, []byte("é")},
		{tea.KeyMsg{Type: tea.KeySpace}, []byte(" ")},
		{tea.KeyMsg{Type: tea.KeyEnter}, []byte("\n")},
		{tea.KeyMsg{Type: tea.KeyTab}, []byte("\t")},
		{tea.KeyMsg{Type: tea.KeyBackspace}, []byte{0x7f}},
		{tea.KeyMsg{Type: tea.KeyEsc}, []byte{0x1b}},
		{tea.KeyMsg{Type: tea.KeyCtrlC}, []byte{3}},
		{tea.KeyMsg{Type: tea.KeyCtrlD}, []byte{4}},
		{tea.KeyMsg{Type: tea.KeyCtrlA}, []byte{1}},
		{tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x"), Alt: true}, []byte{0x1b, 'x'}},
		{tea.KeyMsg{Type: tea.KeyUp}, nil},
		{tea.KeyMsg{Type: tea.KeyF1}, nil},
	}
	for _, c := range cases {
		if got := keyBytes(c.msg); !bytes.Equal(got, c.want) {
			t.Errorf("keyBytes(%s) = %q, want %q", c.msg.String(), got, c.want)
		}
	}
}

func newModel(t *testing.T, run string) (Model, *logbuf.Store) {
	t.Helper()
	cfg := &config.Config{
		Settings: config.Settings{ShutdownTimeout: 5 * time.Second, Scrollback: 1000},
		Processes: []config.Process{{
			Name: "echoer", Run: run, Cwd: t.TempDir(), Stdin: true,
			StopSignal: syscall.SIGTERM, StopTimeout: 5 * time.Second,
		}},
	}
	store := logbuf.NewStore(1, cfg.Settings.Scrollback)
	mgr := proc.NewManager(cfg, store)
	mgr.StartAll()
	t.Cleanup(func() { mgr.Shutdown() })
	m := New(cfg, store, mgr)
	m.selected = 1
	return m, store
}

func press(m Model, msg tea.KeyMsg) (Model, tea.Cmd) {
	next, cmd := m.Update(msg)
	return next.(Model), cmd
}

func key(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func waitLine(t *testing.T, store *logbuf.Store, substr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, l := range store.Proc(0) {
			if l.Kind == logbuf.Stdout && strings.Contains(l.Text, substr) {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("child never printed %q", substr)
}

// odLoop echoes each stdin line as od -c output so ESC bytes are visible.
const odLoop = `while IFS= read -r line; do printf '%s' "$line" | od -An -c; done`

func TestEnterTogglesInputModeAndEscEscLeaves(t *testing.T) {
	m, store := newModel(t, `while IFS= read -r line; do echo "got:$line"; done`)

	// Command mode: a letter is a command, not stdin.
	m, _ = press(m, key("h"))
	if m.input {
		t.Fatal("a plain key must not enter input mode")
	}
	m.selected = 1
	m, _ = press(m, key("enter"))
	if !m.input {
		t.Fatal("enter should turn input mode on")
	}
	m, _ = press(m, key("h"))
	m, _ = press(m, key("i"))
	m, _ = press(m, key("enter"))
	waitLine(t, store, "got:hi")

	m, cmd := press(m, key("esc"))
	if !m.escPending || cmd == nil {
		t.Fatal("first esc should be held with a timeout scheduled")
	}
	m, _ = press(m, key("esc"))
	if m.input || m.escPending {
		t.Fatal("esc esc should leave input mode")
	}
	// The stale timeout must not forward anything now.
	next, _ := m.Update(escTimeoutMsg{seq: m.escSeq})
	m = next.(Model)
	m, _ = press(m, key("q")) // a command again: q starts shutdown
	if !m.shuttingDown {
		t.Fatal("after leaving input mode keys should be commands")
	}
}

func TestLoneEscIsForwardedAfterTimeout(t *testing.T) {
	m, store := newModel(t, odLoop)
	m, _ = press(m, key("enter"))
	m, _ = press(m, key("esc"))
	seq := m.escSeq
	next, _ := m.Update(escTimeoutMsg{seq: seq})
	m = next.(Model)
	if m.escPending || !m.input {
		t.Fatal("timeout should flush the esc and stay in input mode")
	}
	m, _ = press(m, key("enter"))
	waitLine(t, store, "033")
}

func TestEscThenKeyKeepsOrder(t *testing.T) {
	m, store := newModel(t, odLoop)
	m, _ = press(m, key("enter"))
	m, _ = press(m, key("esc"))
	m, _ = press(m, key("a"))
	if m.escPending {
		t.Fatal("a following key should flush the held esc")
	}
	m, _ = press(m, key("enter"))
	waitLine(t, store, "033   a")
}

func TestEnterNeedsAStdinTarget(t *testing.T) {
	m, _ := newModel(t, "sleep 30")
	m.selected = 0
	m, _ = press(m, key("enter"))
	if m.input {
		t.Fatal("enter in the all view must not enter input mode")
	}
	m.selected = 1
	m.cfg.Processes[0].Stdin = false
	m, _ = press(m, key("enter"))
	if m.input {
		t.Fatal("enter on a stdin:false process must not enter input mode")
	}
}

func TestBadgeOverlaysTopRight(t *testing.T) {
	m, _ := newModel(t, "sleep 30")
	next, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 10})
	m = next.(Model)
	m, _ = press(m, key("enter"))
	first, _, _ := strings.Cut(m.View(), "\n")
	if w := lipgloss.Width(first); w != 80 {
		t.Errorf("top line width = %d, want 80", w)
	}
	if !strings.HasSuffix(first, m.modeBadge()) {
		t.Errorf("badge missing from top line: %q", first)
	}
}
