package config

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".parallax.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadOrderAndDefaults(t *testing.T) {
	path := write(t, `
settings:
  shutdown_timeout: 3s
webui:
  run: echo hi
sor:
  run: echo yo
  stop_timeout: 1s
  stop_signal: INT
  cwd: sub
`)
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Settings.ShutdownTimeout != 3*time.Second {
		t.Errorf("shutdown_timeout = %v, want 3s", cfg.Settings.ShutdownTimeout)
	}
	if cfg.Settings.Scrollback != 10000 {
		t.Errorf("scrollback = %d, want default 10000", cfg.Settings.Scrollback)
	}
	if len(cfg.Processes) != 2 || cfg.Processes[0].Name != "webui" || cfg.Processes[1].Name != "sor" {
		t.Fatalf("processes = %+v, want [webui sor] in file order", cfg.Processes)
	}

	webui, sor := cfg.Processes[0], cfg.Processes[1]
	if webui.StopTimeout != 3*time.Second || webui.StopSignal != syscall.SIGTERM {
		t.Errorf("webui should inherit defaults, got %+v", webui)
	}
	if sor.StopTimeout != time.Second || sor.StopSignal != syscall.SIGINT {
		t.Errorf("sor overrides not applied: %+v", sor)
	}
	if sor.Cwd != filepath.Join(filepath.Dir(path), "sub") {
		t.Errorf("cwd = %q, want config-relative sub", sor.Cwd)
	}
}

func TestLoadSubset(t *testing.T) {
	path := write(t, "a:\n  run: echo a\nb:\n  run: echo b\nc:\n  run: echo c\n")
	cfg, err := Load(path, []string{"c", "a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Processes) != 2 || cfg.Processes[0].Name != "a" || cfg.Processes[1].Name != "c" {
		t.Fatalf("subset = %+v, want [a c] in config order", cfg.Processes)
	}
	if _, err := Load(path, []string{"nope"}); err == nil {
		t.Error("unknown subset name should error")
	}
}

func TestLoadErrors(t *testing.T) {
	for name, content := range map[string]string{
		"missing run":    "a:\n  cwd: .\n",
		"empty file":     "",
		"only settings":  "settings:\n  shutdown_timeout: 5s\n",
		"bad duration":   "settings:\n  shutdown_timeout: banana\na:\n  run: echo\n",
		"bad signal":     "a:\n  run: echo\n  stop_signal: SIGWAT\n",
		"top level list": "- a\n- b\n",
	} {
		path := write(t, content)
		if _, err := Load(path, nil); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestParseSignal(t *testing.T) {
	for _, in := range []string{"TERM", "SIGTERM", "sigterm", "term"} {
		sig, err := ParseSignal(in)
		if err != nil || sig != syscall.SIGTERM {
			t.Errorf("ParseSignal(%q) = %v, %v", in, sig, err)
		}
	}
}
