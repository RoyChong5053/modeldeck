package runtime

import (
	"bufio"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// MemAvailableMB reads MemAvailable from /proc/meminfo, in MB.
func MemAvailableMB() int64 {
	return meminfoField("MemAvailable:")
}

func MemTotalMB() int64 {
	return meminfoField("MemTotal:")
}

func meminfoField(key string) int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, key) {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, _ := strconv.ParseInt(fields[1], 10, 64)
				return kb / 1024
			}
		}
	}
	return 0
}

// GroupRSSKB sums VmRSS of every process whose process group id == pgid.
func GroupRSSKB(pgid int) int64 {
	if pgid <= 0 {
		return 0
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	var total int64
	for _, de := range entries {
		if !de.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(de.Name())
		if err != nil {
			continue
		}
		if procGroup(pid) != pgid {
			continue
		}
		if rss := pidRSSKB(pid); rss > 0 {
			total += rss
		}
	}
	return total
}

func procGroup(pid int) int {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	// Format: pid (comm) state ppid pgrp ...
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 || i+2 >= len(s) {
		return 0
	}
	fields := strings.Fields(s[i+2:])
	if len(fields) < 3 {
		return 0
	}
	pgrp, _ := strconv.Atoi(fields[2])
	return pgrp
}

func pidRSSKB(pid int) int64 {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, _ := strconv.ParseInt(fields[1], 10, 64)
				return kb
			}
		}
	}
	return 0
}

// GPUStats is a thin wrapper over nvidia-smi. ok is false when no GPU/tool.
type GPUStats struct {
	OK      bool
	FreeMB  int64
	TotalMB int64
	PerPID  map[int]int64
}

var gpuMu sync.Mutex

func ReadGPU() GPUStats {
	gpuMu.Lock()
	defer gpuMu.Unlock()
	out := GPUStats{PerPID: map[int]int64{}}
	// Free / total
	b, err := exec.Command("nvidia-smi",
		"--query-gpu=memory.free,memory.total",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return out
	}
	line := strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0])
	parts := strings.Split(line, ",")
	if len(parts) >= 2 {
		out.FreeMB, _ = strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
		out.TotalMB, _ = strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	}
	out.OK = true

	// Per-process usage
	b, err = exec.Command("nvidia-smi",
		"--query-compute-apps=pid,used_memory",
		"--format=csv,noheader,nounits").Output()
	if err == nil {
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			l = strings.TrimSpace(l)
			if l == "" {
				continue
			}
			parts := strings.Split(l, ",")
			if len(parts) < 2 {
				continue
			}
			pid, err := strconv.Atoi(strings.TrimSpace(parts[0]))
			if err != nil {
				continue
			}
			mb, _ := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
			out.PerPID[pid] += mb
		}
	}
	return out
}
