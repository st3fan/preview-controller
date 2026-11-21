package dashboard

import (
	"fmt"
	"io/fs"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type capacity struct {
	Used, Total, Available string
	Percent                int
}

func usage(total, free, available uint64) *capacity {
	if total == 0 || free > total || available > free {
		return nil
	}
	gib := func(n uint64) string { return fmt.Sprintf("%.1f", float64(n)/(1<<30)) }
	used := total - free
	return &capacity{
		Used: gib(used), Total: gib(total), Available: gib(available),
		Percent: int(math.Round(float64(used) * 100 / float64(total))),
	}
}

type cpuLoad struct {
	One, Five, Fifteen string
	CPUs               int
}

type hostSnapshot struct {
	Memory, Disk *capacity
	Load         *cpuLoad
	At           time.Time
}

func (s hostSnapshot) Age() int64 { return int64(time.Since(s.At).Seconds()) }

func newHostSampler(workspace string) func() hostSnapshot {
	var mu sync.Mutex
	var cached hostSnapshot
	return func() hostSnapshot {
		mu.Lock()
		defer mu.Unlock()
		if time.Since(cached.At) >= 15*time.Second {
			proc := os.DirFS("/proc")
			cached = hostSnapshot{At: time.Now().UTC(), Memory: readMemory(proc), Disk: diskUsage(workspace), Load: readLoad(proc)}
		}
		return cached
	}
}

func readMemory(proc fs.FS) *capacity {
	data, err := fs.ReadFile(proc, "meminfo")
	if err != nil {
		return nil
	}
	values := map[string]uint64{}
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || (fields[0] != "MemTotal:" && fields[0] != "MemAvailable:") || fields[2] != "kB" {
			continue
		}
		n, err := strconv.ParseUint(fields[1], 10, 54)
		if err == nil {
			values[fields[0]] = n * 1024
		}
	}
	available, ok := values["MemAvailable:"]
	if !ok {
		return nil
	}
	return usage(values["MemTotal:"], available, available)
}

func readLoad(proc fs.FS) *cpuLoad {
	raw, err := fs.ReadFile(proc, "loadavg")
	f := strings.Fields(string(raw))
	if err != nil || len(f) < 3 {
		return nil
	}
	for _, value := range f[:3] {
		n, err := strconv.ParseFloat(value, 64)
		if err != nil || n < 0 || math.IsNaN(n) || math.IsInf(n, 0) {
			return nil
		}
	}
	stat, err := fs.ReadFile(proc, "stat")
	if err != nil {
		return nil
	}
	var cpus int
	for line := range strings.SplitSeq(string(stat), "\n") {
		name, _, _ := strings.Cut(line, " ")
		if id, ok := strings.CutPrefix(name, "cpu"); ok {
			if _, err := strconv.ParseUint(id, 10, 32); err == nil {
				cpus++
			}
		}
	}
	if cpus == 0 {
		return nil
	}
	return &cpuLoad{f[0], f[1], f[2], cpus}
}
