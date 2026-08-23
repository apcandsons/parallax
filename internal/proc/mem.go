package proc

import (
	"os/exec"
	"strconv"
	"strings"
)

// RSS returns resident-set bytes per process, summed over each run's whole
// process group (so a `make` entry includes whatever it spawned). Zero for
// processes that aren't running or can't be measured. One ps fork per call;
// callers should sample on a timer, not per frame.
func (m *Manager) RSS() []int64 {
	out := make([]int64, len(m.Procs))
	pgids := make(map[int]int, len(m.Procs)) // pgid -> proc index
	for i, p := range m.Procs {
		if pg := p.Pgid(); pg > 0 {
			pgids[pg] = i
		}
	}
	if len(pgids) == 0 {
		return out
	}
	// Same flags work on macOS and Linux; rss is in KiB.
	b, err := exec.Command("ps", "-axo", "pgid=,rss=").Output()
	if err != nil {
		return out
	}
	for line := range strings.Lines(string(b)) {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		pgid, err1 := strconv.Atoi(f[0])
		rss, err2 := strconv.ParseInt(f[1], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		if i, ok := pgids[pgid]; ok {
			out[i] += rss * 1024
		}
	}
	return out
}
