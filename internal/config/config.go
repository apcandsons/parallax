// Package config parses .parallax.yaml: a flat map of process name →
// definition, plus a reserved "settings" key. Process order in the file is
// preserved; it determines display order and color assignment.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

type Settings struct {
	ShutdownTimeout time.Duration
	Scrollback      int
}

// Ready is a readiness probe. Exactly one of the fields is set.
type Ready struct {
	TCP  string         // host:port; ready when a connect succeeds
	HTTP string         // URL; ready on a 2xx response
	Log  *regexp.Regexp // ready when a line of the process's own output matches
}

func (r *Ready) String() string {
	switch {
	case r == nil:
		return "none"
	case r.TCP != "":
		return "tcp " + r.TCP
	case r.HTTP != "":
		return "http " + r.HTTP
	default:
		return "log /" + r.Log.String() + "/"
	}
}

type Process struct {
	Name         string
	Run          string
	Cwd          string
	Env          map[string]string
	Color        string
	StopSignal   syscall.Signal
	StopTimeout  time.Duration
	Ready        *Ready        // nil: ready as soon as started
	ReadyTimeout time.Duration // how long the probe may take before the process is marked failed
	WaitFor      []string      // names of processes that must be ready before this one starts
}

type Config struct {
	Settings  Settings
	Processes []Process
}

type settingsYAML struct {
	ShutdownTimeout string `yaml:"shutdown_timeout"`
	Scrollback      *int   `yaml:"scrollback"`
}

type processYAML struct {
	Run          string            `yaml:"run"`
	Cwd          string            `yaml:"cwd"`
	Env          map[string]string `yaml:"env"`
	Color        string            `yaml:"color"`
	StopSignal   string            `yaml:"stop_signal"`
	StopTimeout  string            `yaml:"stop_timeout"`
	Restart      string            `yaml:"restart"` // reserved for a future version; parsed but ignored
	Ready        *readyYAML        `yaml:"ready"`
	ReadyTimeout string            `yaml:"ready_timeout"`
	WaitFor      []string          `yaml:"wait_for"`
}

type readyYAML struct {
	TCP  string `yaml:"tcp"`
	HTTP string `yaml:"http"`
	Log  string `yaml:"log"`
}

const defaultReadyTimeout = 60 * time.Second

// Load reads the config at path. If only is non-empty, the result is
// restricted to those process names (config-file order is kept regardless).
func Load(path string, only []string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	baseDir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, err
	}

	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	cfg := &Config{Settings: Settings{
		ShutdownTimeout: 10 * time.Second,
		Scrollback:      10000,
	}}

	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return nil, fmt.Errorf("%s: no process definitions", path)
	}
	m := root.Content[0]
	if m.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: top level must be a mapping of process names", path)
	}

	type rawProc struct {
		name string
		line int
		def  processYAML
	}
	var raws []rawProc
	seen := map[string]bool{}
	for i := 0; i+1 < len(m.Content); i += 2 {
		key, val := m.Content[i], m.Content[i+1]
		if seen[key.Value] {
			return nil, fmt.Errorf("%s:%d: duplicate key %q", path, key.Line, key.Value)
		}
		seen[key.Value] = true

		if key.Value == "settings" {
			var sy settingsYAML
			if err := val.Decode(&sy); err != nil {
				return nil, fmt.Errorf("%s:%d: settings: %w", path, val.Line, err)
			}
			if sy.ShutdownTimeout != "" {
				d, err := time.ParseDuration(sy.ShutdownTimeout)
				if err != nil {
					return nil, fmt.Errorf("%s: settings.shutdown_timeout: %w", path, err)
				}
				cfg.Settings.ShutdownTimeout = d
			}
			if sy.Scrollback != nil {
				if *sy.Scrollback < 1 {
					return nil, fmt.Errorf("%s: settings.scrollback must be at least 1", path)
				}
				cfg.Settings.Scrollback = *sy.Scrollback
			}
			continue
		}

		var py processYAML
		if err := val.Decode(&py); err != nil {
			return nil, fmt.Errorf("%s:%d: process %q: %w", path, val.Line, key.Value, err)
		}
		raws = append(raws, rawProc{name: key.Value, line: key.Line, def: py})
	}

	for _, r := range raws {
		if strings.TrimSpace(r.def.Run) == "" {
			return nil, fmt.Errorf("%s:%d: process %q: run is required", path, r.line, r.name)
		}
		p := Process{
			Name:         r.name,
			Run:          r.def.Run,
			Cwd:          baseDir,
			Env:          r.def.Env,
			Color:        r.def.Color,
			StopSignal:   syscall.SIGTERM,
			StopTimeout:  cfg.Settings.ShutdownTimeout,
			ReadyTimeout: defaultReadyTimeout,
		}
		if r.def.Cwd != "" {
			if filepath.IsAbs(r.def.Cwd) {
				p.Cwd = r.def.Cwd
			} else {
				p.Cwd = filepath.Join(baseDir, r.def.Cwd)
			}
		}
		if r.def.StopSignal != "" {
			sig, err := ParseSignal(r.def.StopSignal)
			if err != nil {
				return nil, fmt.Errorf("%s: process %q: %w", path, r.name, err)
			}
			p.StopSignal = sig
		}
		if r.def.StopTimeout != "" {
			d, err := time.ParseDuration(r.def.StopTimeout)
			if err != nil {
				return nil, fmt.Errorf("%s: process %q: stop_timeout: %w", path, r.name, err)
			}
			p.StopTimeout = d
		}
		if r.def.Ready != nil {
			ready, err := parseReady(r.def.Ready)
			if err != nil {
				return nil, fmt.Errorf("%s: process %q: ready: %w", path, r.name, err)
			}
			p.Ready = ready
		}
		if r.def.ReadyTimeout != "" {
			d, err := time.ParseDuration(r.def.ReadyTimeout)
			if err != nil {
				return nil, fmt.Errorf("%s: process %q: ready_timeout: %w", path, r.name, err)
			}
			p.ReadyTimeout = d
		}
		for _, dep := range r.def.WaitFor {
			if dep == r.name {
				return nil, fmt.Errorf("%s: process %q: wait_for: cannot wait for itself", path, r.name)
			}
			if !seen[dep] || dep == "settings" {
				return nil, fmt.Errorf("%s: process %q: wait_for: unknown process %q", path, r.name, dep)
			}
			p.WaitFor = append(p.WaitFor, dep)
		}
		cfg.Processes = append(cfg.Processes, p)
	}

	if cycle := findCycle(cfg.Processes); cycle != nil {
		return nil, fmt.Errorf("%s: wait_for cycle: %s", path, strings.Join(cycle, " -> "))
	}

	if len(only) > 0 {
		want := map[string]bool{}
		for _, name := range only {
			want[name] = true
		}
		var kept []Process
		for _, p := range cfg.Processes {
			if want[p.Name] {
				kept = append(kept, p)
				delete(want, p.Name)
			}
		}
		if len(want) > 0 {
			var missing []string
			for name := range want {
				missing = append(missing, name)
			}
			return nil, fmt.Errorf("%s: unknown process(es): %s", path, strings.Join(missing, ", "))
		}
		cfg.Processes = kept
		// Waits on processes that aren't part of this run would never be
		// satisfied, so they are dropped.
		running := map[string]bool{}
		for _, p := range kept {
			running[p.Name] = true
		}
		for i := range kept {
			var deps []string
			for _, dep := range kept[i].WaitFor {
				if running[dep] {
					deps = append(deps, dep)
				}
			}
			kept[i].WaitFor = deps
		}
	}

	if len(cfg.Processes) == 0 {
		return nil, fmt.Errorf("%s: no process definitions", path)
	}
	return cfg, nil
}

func parseReady(ry *readyYAML) (*Ready, error) {
	set := 0
	for _, v := range []string{ry.TCP, ry.HTTP, ry.Log} {
		if v != "" {
			set++
		}
	}
	if set != 1 {
		return nil, fmt.Errorf("exactly one of tcp, http, log is required")
	}
	r := &Ready{TCP: ry.TCP, HTTP: ry.HTTP}
	if ry.HTTP != "" && !strings.HasPrefix(ry.HTTP, "http://") && !strings.HasPrefix(ry.HTTP, "https://") {
		return nil, fmt.Errorf("http: %q must start with http:// or https://", ry.HTTP)
	}
	if ry.Log != "" {
		re, err := regexp.Compile(ry.Log)
		if err != nil {
			return nil, fmt.Errorf("log: %w", err)
		}
		r.Log = re
	}
	return r, nil
}

// findCycle returns a wait_for cycle as a name path (first == last), or nil.
func findCycle(procs []Process) []string {
	deps := map[string][]string{}
	for _, p := range procs {
		deps[p.Name] = p.WaitFor
	}
	const (
		white = iota
		gray
		black
	)
	color := map[string]int{}
	var stack []string
	var visit func(name string) []string
	visit = func(name string) []string {
		color[name] = gray
		stack = append(stack, name)
		for _, d := range deps[name] {
			switch color[d] {
			case gray:
				for i, s := range stack {
					if s == d {
						return append(append([]string{}, stack[i:]...), d)
					}
				}
			case white:
				if c := visit(d); c != nil {
					return c
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[name] = black
		return nil
	}
	for _, p := range procs {
		if color[p.Name] == white {
			if c := visit(p.Name); c != nil {
				return c
			}
		}
	}
	return nil
}

var signals = map[string]syscall.Signal{
	"HUP":  syscall.SIGHUP,
	"INT":  syscall.SIGINT,
	"QUIT": syscall.SIGQUIT,
	"KILL": syscall.SIGKILL,
	"TERM": syscall.SIGTERM,
	"USR1": syscall.SIGUSR1,
	"USR2": syscall.SIGUSR2,
}

func ParseSignal(name string) (syscall.Signal, error) {
	key := strings.TrimPrefix(strings.ToUpper(name), "SIG")
	if sig, ok := signals[key]; ok {
		return sig, nil
	}
	return 0, fmt.Errorf("unsupported signal %q", name)
}

func SignalName(sig syscall.Signal) string {
	for name, s := range signals {
		if s == sig {
			return "SIG" + name
		}
	}
	return fmt.Sprintf("signal %d", int(sig))
}
