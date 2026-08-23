// Package ui is the bubbletea model: a log viewport on top, the process
// selector bar on the bottom.
package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/reflow/truncate"
	"github.com/muesli/reflow/wrap"

	"github.com/apcandsons/parallax/internal/config"
	"github.com/apcandsons/parallax/internal/logbuf"
	"github.com/apcandsons/parallax/internal/proc"
)

// RefreshMsg tells the UI new log lines are available.
type RefreshMsg struct{}

// StopRequestMsg triggers graceful shutdown, e.g. from an external SIGTERM.
type StopRequestMsg struct{}

type shutdownDoneMsg struct{}
type tickMsg time.Time
type memMsg []int64 // per-process RSS bytes, from proc.Manager.RSS

var (
	tsStyle       = lipgloss.NewStyle().Faint(true)
	eventStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("8")) // gray
	errEventStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("9")) // red
	okMarkStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

// palette cycles by config order: cyan, green, magenta, yellow, blue, ...
var palette = []string{"6", "2", "5", "3", "4", "13", "10", "14"}

var namedColors = map[string]string{
	"black": "0", "red": "1", "green": "2", "yellow": "3", "blue": "4",
	"magenta": "5", "purple": "5", "cyan": "6", "white": "7", "gray": "8",
	"grey": "8", "orange": "214", "amber": "214", "pink": "213",
}

type Model struct {
	cfg   *config.Config
	store *logbuf.Store
	mgr   *proc.Manager

	procStyles []lipgloss.Style
	nameWidth  int

	selected      int     // 0 = all, 1..n = process index+1
	stopping      []bool  // per-process: ctrl-x pressed once for the current run
	mem           []int64 // per-process RSS bytes; nil until the first sample
	follow        bool
	vp            viewport.Model
	ready         bool
	width, height int

	shuttingDown bool
	forcing      bool
	deadline     time.Time
}

func New(cfg *config.Config, store *logbuf.Store, mgr *proc.Manager) Model {
	m := Model{cfg: cfg, store: store, mgr: mgr, follow: true,
		stopping: make([]bool, len(cfg.Processes))}
	for i, p := range cfg.Processes {
		m.procStyles = append(m.procStyles, lipgloss.NewStyle().Foreground(colorFor(p.Color, i)))
		if w := len(p.Name) + 1; w > m.nameWidth { // +1 for the colon
			m.nameWidth = w
		}
	}
	return m
}

func colorFor(override string, i int) lipgloss.Color {
	if override != "" {
		if v, ok := namedColors[strings.ToLower(override)]; ok {
			return lipgloss.Color(v)
		}
		return lipgloss.Color(override) // "214", "#ff8800", ...
	}
	return lipgloss.Color(palette[i%len(palette)])
}

func (m Model) Init() tea.Cmd {
	go m.mgr.StartAll()
	return tea.Batch(tick(), sampleMem(m.mgr, time.Second))
}

func tick() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// sampleMem runs ps off the UI goroutine after the delay; the result comes
// back as a memMsg, which schedules the next sample.
func sampleMem(mgr *proc.Manager, delay time.Duration) tea.Cmd {
	return tea.Tick(delay, func(time.Time) tea.Msg { return memMsg(mgr.RSS()) })
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// A pty can report 0x0 (e.g. under `script`); a non-positive
		// viewport height makes bubbles/viewport panic.
		m.width = max(msg.Width, 20)
		m.height = max(msg.Height, 2)
		if !m.ready {
			m.vp = viewport.New(m.width, m.height-1)
			m.ready = true
		} else {
			m.vp.Width = m.width
			m.vp.Height = m.height - 1
		}
		m.rebuild()
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.MouseMsg:
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		m.follow = m.vp.AtBottom()
		return m, cmd

	case RefreshMsg:
		m.rebuild()
		return m, nil

	case StopRequestMsg:
		return m.requestStop()

	case tickMsg:
		return m, tick() // countdown and liveness markers re-render via View

	case memMsg:
		m.mem = msg
		return m, sampleMem(m.mgr, 2*time.Second)

	case shutdownDoneMsg:
		return m, tea.Quit
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch s := msg.String(); s {
	case "ctrl+c", "q":
		return m.requestStop()

	case "tab", "right", "l":
		m.selected = (m.selected + 1) % (len(m.cfg.Processes) + 1)
		m.follow = true
		m.rebuild()

	case "shift+tab", "left", "h":
		m.selected = (m.selected + len(m.cfg.Processes)) % (len(m.cfg.Processes) + 1)
		m.follow = true
		m.rebuild()

	case "f", "end":
		m.follow = true
		m.vp.GotoBottom()

	case "r":
		if m.selected > 0 {
			i := m.selected - 1
			m.stopping[i] = false
			go m.mgr.Restart(i)
		}

	case "ctrl+x":
		if m.selected > 0 {
			i := m.selected - 1
			if !m.mgr.Procs[i].Status().Running {
				m.stopping[i] = false
			} else if !m.stopping[i] {
				m.stopping[i] = true
				go m.mgr.Stop(i)
			} else {
				go m.mgr.ForceKill(i)
			}
		}

	case "up", "down", "pgup", "pgdown", "k", "j", "ctrl+u", "ctrl+d", "home":
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		m.follow = m.vp.AtBottom()
		return m, cmd

	default:
		if len(s) == 1 && s[0] >= '0' && s[0] <= '9' {
			if n := int(s[0] - '0'); n <= len(m.cfg.Processes) {
				m.selected = n
				m.follow = true
				m.rebuild()
			}
		}
	}
	return m, nil
}

func (m Model) requestStop() (tea.Model, tea.Cmd) {
	if m.shuttingDown {
		// Second ctrl-c: skip the grace period.
		if !m.forcing {
			m.forcing = true
			go m.mgr.ForceKillAll()
		}
		return m, nil
	}
	m.shuttingDown = true
	timeout := time.Duration(0)
	for _, p := range m.cfg.Processes {
		if p.StopTimeout > timeout {
			timeout = p.StopTimeout
		}
	}
	m.deadline = time.Now().Add(timeout)
	mgr := m.mgr
	return m, func() tea.Msg {
		mgr.Shutdown()
		return shutdownDoneMsg{}
	}
}

func (m *Model) rebuild() {
	if !m.ready {
		return
	}
	var lines []logbuf.Line
	if m.selected == 0 {
		lines = m.store.All()
	} else {
		lines = m.store.Proc(m.selected - 1)
	}
	var b strings.Builder
	for i, l := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(m.renderLine(l))
	}
	content := b.String()
	if m.width > 0 {
		content = wrap.String(content, m.width)
	}
	m.vp.SetContent(content)
	if m.follow {
		m.vp.GotoBottom()
	}
}

func (m *Model) renderLine(l logbuf.Line) string {
	ts := tsStyle.Render("[" + l.Time.Format("2006-01-02 15:04:05.000") + "]")
	name := m.cfg.Processes[l.Proc].Name + ":"
	if pad := m.nameWidth - len(name); pad > 0 {
		name += strings.Repeat(" ", pad)
	}
	name = m.procStyles[l.Proc].Render(name)

	// stderr is left unstyled: most tools log routine output there, so
	// coloring it amber painted whole panes (see doc/000-design.md).
	text := l.Text
	switch l.Kind {
	case logbuf.Event:
		text = eventStyle.Render(text)
	case logbuf.ErrEvent:
		text = errEventStyle.Render(text)
	}
	return ts + " " + name + " " + text
}

func (m Model) View() string {
	if !m.ready {
		return "starting..."
	}
	return m.vp.View() + "\n" + m.barView()
}

func (m Model) barView() string {
	parts := make([]string, 0, len(m.cfg.Processes)+1)
	parts = append(parts, m.slot(0, "all", nil))
	for i, p := range m.cfg.Processes {
		st := m.mgr.Procs[i].Status()
		parts = append(parts, m.slot(i+1, p.Name, &st))
	}
	bar := strings.Join(parts, " ")

	if m.shuttingDown {
		label := "  killing..."
		if !m.forcing {
			remain := time.Until(m.deadline)
			if remain < 0 {
				remain = 0
			}
			secs := int((remain + 999*time.Millisecond) / time.Second)
			label = fmt.Sprintf("  shutting down... %ds (ctrl-c again to kill now)", secs)
		}
		bar += errEventStyle.Render(label)
	} else if i := m.selected - 1; i >= 0 && m.stopping[i] && m.mgr.Procs[i].Status().Running {
		bar += errEventStyle.Render("  stopping... (ctrl-x again to kill)")
	}

	if m.width > 0 {
		bar = truncate.String(bar, uint(m.width))
	}
	return bar
}

func (m Model) slot(idx int, name string, st *proc.Status) string {
	label := fmt.Sprintf("%d-%s", idx, name)
	if mem := m.slotMem(idx, st); mem > 0 {
		label += ":" + fmtMem(mem)
	}
	style := lipgloss.NewStyle()
	if st != nil {
		style = m.procStyles[idx-1]
		if st.Exited {
			switch {
			case st.ExitCode == 0:
				label += " " + "✓"
				style = okMarkStyle
			case st.ExitCode > 0:
				label += fmt.Sprintf(" ✗%d", st.ExitCode)
				style = errEventStyle
			default:
				label += " ✗" + strings.TrimPrefix(st.Signal, "SIG")
				style = errEventStyle
			}
		}
	}
	if idx == m.selected {
		style = style.Reverse(true).Bold(true)
	}
	return style.Render("[" + label + "]")
}

// slotMem is the RSS to show for a slot: the process's own for 1..n (only
// while running — a stale sample can outlive the process), the sum for "all".
func (m Model) slotMem(idx int, st *proc.Status) int64 {
	if idx == 0 {
		var total int64
		for i := range m.mem {
			if m.mgr.Procs[i].Status().Running {
				total += m.mem[i]
			}
		}
		return total
	}
	if idx-1 < len(m.mem) && st != nil && st.Running {
		return m.mem[idx-1]
	}
	return 0
}

func fmtMem(b int64) string {
	mb := float64(b) / (1 << 20)
	if mb >= 1024 {
		return fmt.Sprintf("%.1fGB", mb/1024)
	}
	return fmt.Sprintf("%.0fMB", mb)
}
