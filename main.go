// parallax runs multiple long-lived processes from one terminal and
// multiplexes their logs into a TUI. See doc/000-design.md.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/apcandsons/parallax/internal/config"
	"github.com/apcandsons/parallax/internal/logbuf"
	"github.com/apcandsons/parallax/internal/proc"
	"github.com/apcandsons/parallax/internal/ui"
)

func main() {
	configPath := flag.String("f", ".parallax.yaml", "path to config file")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: parallax [-f config.yaml] [process ...]\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	cfg, err := config.Load(*configPath, flag.Args())
	if err != nil {
		fmt.Fprintln(os.Stderr, "parallax:", err)
		os.Exit(1)
	}

	store := logbuf.NewStore(len(cfg.Processes), cfg.Settings.Scrollback)
	mgr := proc.NewManager(cfg, store)

	p := tea.NewProgram(ui.New(cfg, store, mgr),
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)

	go func() {
		for range store.Notify() {
			p.Send(ui.RefreshMsg{})
		}
	}()

	// Ctrl-c arrives as a key event (raw mode), and the children are in their
	// own process groups, so the only signal to handle here is an external
	// SIGTERM to parallax itself.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM)
	go func() {
		for range sigCh {
			p.Send(ui.StopRequestMsg{})
		}
	}()

	_, runErr := p.Run()
	mgr.ForceKillAll() // no-op if shutdown already completed; safety net otherwise
	if runErr != nil {
		fmt.Fprintln(os.Stderr, "parallax:", runErr)
		os.Exit(1)
	}
	if mgr.Failed() {
		os.Exit(1)
	}
}
