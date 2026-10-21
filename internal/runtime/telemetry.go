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
	UsedMB  int64
	TotalMB int64
	Name    string
	UtilPct int64
	TempC   int64
	PerPID  map[int]int64
}

var gpuMu sync.Mutex

func ReadGPU() GPUStats {
	gpuMu.Lock()
	defer gpuMu.Unlock()
	out := GPUStats{PerPID: map[int]int64{}}
	// One query for the card facts: name, used, free, total, util, temp.
	b, err := exec.Command("nvidia-smi",
		"--query-gpu=name,memory.used,memory.free,memory.total,utilization.gpu,temperature.gpu",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return out
	}
	line := strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0])
	if line == "" {
		return out
	}
	parts := strings.Split(line, ",")
	if len(parts) < 6 {
		return out
	}
	// The name may itself contain commas: keep the leading fields as the name.
	out.Name = strings.TrimSpace(strings.Join(parts[:len(parts)-5], ","))
	out.UsedMB, _ = strconv.ParseInt(strings.TrimSpace(parts[len(parts)-5]), 10, 64)
	out.FreeMB, _ = strconv.ParseInt(strings.TrimSpace(parts[len(parts)-4]), 10, 64)
	out.TotalMB, _ = strconv.ParseInt(strings.TrimSpace(parts[len(parts)-3]), 10, 64)
	out.UtilPct, _ = strconv.ParseInt(strings.TrimSpace(parts[len(parts)-2]), 10, 64)
	out.TempC, _ = strconv.ParseInt(strings.TrimSpace(parts[len(parts)-1]), 10, 64)
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

// CPUInfo describes the host CPU.
type CPUInfo struct {
	Model string  `json:"model"`
	Cores int     `json:"cores"`
	Load1 float64 `json:"load1"`
}

// MemInfo describes host memory.
type MemInfo struct {
	TotalMB     int64 `json:"total_mb"`
	AvailableMB int64 `json:"available_mb"`
	UsedMB      int64 `json:"used_mb"`
}

// HostInfo is the identity and the hardware status of the machine deck runs on.
// It is what lets the same WebUI be told apart across several devices.
type HostInfo struct {
	Hostname string   `json:"hostname"`
	Label    string   `json:"label,omitempty"`
	GPU      GPUStats `json:"gpu"`
	CPU      CPUInfo  `json:"cpu"`
	Mem      MemInfo  `json:"mem"`
	UptimeS  int64    `json:"uptime_s"`
	Version  string   `json:"version"`
}

// ReadCPU reads the CPU model, core count and 1-minute load average.
func ReadCPU() CPUInfo {
	out := CPUInfo{}
	b, err := os.ReadFile("/proc/cpuinfo")
	if err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			switch {
			case out.Model == "" && strings.HasPrefix(line, "model name"):
				if i := strings.Index(line, ":"); i >= 0 {
					out.Model = strings.TrimSpace(line[i+1:])
				}
			case strings.HasPrefix(line, "processor"):
				out.Cores++
			}
		}
	}
	if out.Model == "" {
		out.Model = "unknown CPU"
	}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		fields := strings.Fields(string(b))
		if len(fields) > 0 {
			out.Load1, _ = strconv.ParseFloat(fields[0], 64)
		}
	}
	return out
}

// ReadMem reads total/available/used memory in MB.
func ReadMem() MemInfo {
	total := MemTotalMB()
	avail := MemAvailableMB()
	return MemInfo{TotalMB: total, AvailableMB: avail, UsedMB: total - avail}
}

// ReadUptime returns the host uptime in seconds.
func ReadUptime() int64 {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0
	}
	secs, _ := strconv.ParseFloat(fields[0], 64)
	return int64(secs)
}
