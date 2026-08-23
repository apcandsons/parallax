// Package config parses .parallax.yaml: a flat map of process name →
// definition, plus a reserved "settings" key. Process order in the file is
// preserved; it determines display order and color assignment.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

type Settings struct {
	ShutdownTimeout time.Duration
	Scrollback      int
}

type Process struct {
	Name        string
	Run         string
	Cwd         string
	Env         map[string]string
	Color       string
	StopSignal  syscall.Signal
	StopTimeout time.Duration
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
	Run         string            `yaml:"run"`
	Cwd         string            `yaml:"cwd"`
	Env         map[string]string `yaml:"env"`
	Color       string            `yaml:"color"`
	StopSignal  string            `yaml:"stop_signal"`
	StopTimeout string            `yaml:"stop_timeout"`
	Restart     string            `yaml:"restart"` // reserved for a future version; parsed but ignored
}

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
			Name:        r.name,
			Run:         r.def.Run,
			Cwd:         baseDir,
			Env:         r.def.Env,
			Color:       r.def.Color,
			StopSignal:  syscall.SIGTERM,
			StopTimeout: cfg.Settings.ShutdownTimeout,
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
		cfg.Processes = append(cfg.Processes, p)
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
	}

	if len(cfg.Processes) == 0 {
		return nil, fmt.Errorf("%s: no process definitions", path)
	}
	return cfg, nil
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
