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

func TestLoadReadiness(t *testing.T) {
	path := write(t, `
iam:
  run: echo iam
  ready: { tcp: 127.0.0.1:17001 }
  ready_timeout: 5s
db:
  run: echo db
  ready: { log: "listening on" }
api:
  run: echo api
  wait_for: [iam, db]
web:
  run: echo web
  ready: { http: http://127.0.0.1:8080/healthz }
  wait_for: [api]
`)
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	iam, db, api, web := cfg.Processes[0], cfg.Processes[1], cfg.Processes[2], cfg.Processes[3]
	if iam.Ready == nil || iam.Ready.TCP != "127.0.0.1:17001" || iam.ReadyTimeout != 5*time.Second {
		t.Errorf("iam = %+v", iam)
	}
	if db.Ready == nil || db.Ready.Log == nil || !db.Ready.Log.MatchString("now listening on :1") {
		t.Errorf("db = %+v", db)
	}
	if db.ReadyTimeout != defaultReadyTimeout {
		t.Errorf("db.ReadyTimeout = %v, want default", db.ReadyTimeout)
	}
	if api.Ready != nil || len(api.WaitFor) != 2 || api.WaitFor[0] != "iam" || api.WaitFor[1] != "db" {
		t.Errorf("api = %+v", api)
	}
	if web.Ready == nil || web.Ready.HTTP == "" {
		t.Errorf("web = %+v", web)
	}

	// Running a subset drops waits on processes that aren't part of the run.
	sub, err := Load(path, []string{"api", "iam"})
	if err != nil {
		t.Fatal(err)
	}
	if got := sub.Processes[1].WaitFor; len(got) != 1 || got[0] != "iam" {
		t.Errorf("subset api.WaitFor = %v, want [iam]", got)
	}
}

func TestLoadReadinessErrors(t *testing.T) {
	for name, content := range map[string]string{
		"unknown target": "a:\n  run: echo\n  wait_for: [zzz]\n",
		"self wait":      "a:\n  run: echo\n  wait_for: [a]\n",
		"settings wait":  "settings:\n  scrollback: 5\na:\n  run: echo\n  wait_for: [settings]\n",
		"cycle":          "a:\n  run: echo\n  wait_for: [b]\nb:\n  run: echo\n  wait_for: [c]\nc:\n  run: echo\n  wait_for: [a]\n",
		"empty ready":    "a:\n  run: echo\n  ready: {}\n",
		"two probes":     "a:\n  run: echo\n  ready: { tcp: 'x:1', log: y }\n",
		"bad regexp":     "a:\n  run: echo\n  ready: { log: '(' }\n",
		"bad http":       "a:\n  run: echo\n  ready: { http: 'localhost:80' }\n",
		"bad timeout":    "a:\n  run: echo\n  ready_timeout: soon\n",
	} {
		path := write(t, content)
		if _, err := Load(path, nil); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
