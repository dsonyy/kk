package main

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Sample is machine load at one moment, in percent. GPU is nil when the machine reports none.
type Sample struct {
	Time time.Time `json:"time"`
	CPU  float64   `json:"cpu"`
	RAM  float64   `json:"ram"`
	GPU  *float64  `json:"gpu"`
}

const (
	statsEvery = 2 * time.Second
	statsKeep  = 150
)

var stats = struct {
	sync.Mutex
	samples []Sample
	// Linux reports CPU time as counters, so a reading needs the one before it.
	idle, total float64
}{}

func output(name string, args ...string) string {
	out, _ := exec.Command(name, args...).Output()
	return string(out)
}

func number(s string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimRight(strings.TrimSpace(s), "."), 64)
	return f
}

var (
	vmPages    = regexp.MustCompile(`Pages (active|wired down|occupied by compressor):\s+(\d+)`)
	gpuPercent = regexp.MustCompile(`"Device Utilization %"=(\d+)`)
)

func sample() Sample {
	s := Sample{Time: time.Now()}
	gpu := func(v float64) { s.GPU = &v }
	switch runtime.GOOS {
	case "darwin":
		for _, f := range strings.Fields(output("ps", "-A", "-o", "%cpu=")) {
			s.CPU += number(f)
		}
		s.CPU /= float64(runtime.NumCPU())
		var used float64
		for _, m := range vmPages.FindAllStringSubmatch(output("vm_stat"), -1) {
			used += number(m[2])
		}
		if total := number(output("sysctl", "-n", "hw.memsize")); total > 0 {
			s.RAM = used * number(output("sysctl", "-n", "hw.pagesize")) / total * 100
		}
		if m := gpuPercent.FindStringSubmatch(output("ioreg", "-r", "-d", "1", "-w", "0", "-c", "IOAccelerator")); m != nil {
			gpu(number(m[1]))
		}
	case "linux":
		data, _ := os.ReadFile("/proc/stat")
		if line, _, ok := strings.Cut(string(data), "\n"); ok {
			var idle, total float64
			for i, f := range strings.Fields(line)[1:] {
				total += number(f)
				if i == 3 || i == 4 {
					idle += number(f)
				}
			}
			if d := total - stats.total; stats.total > 0 && d > 0 {
				s.CPU = (1 - (idle-stats.idle)/d) * 100
			}
			stats.idle, stats.total = idle, total
		}
		mem := map[string]float64{}
		data, _ = os.ReadFile("/proc/meminfo")
		for _, line := range strings.Split(string(data), "\n") {
			if k, v, ok := strings.Cut(line, ":"); ok {
				mem[k] = number(strings.TrimSuffix(strings.TrimSpace(v), " kB"))
			}
		}
		if mem["MemTotal"] > 0 {
			s.RAM = (1 - mem["MemAvailable"]/mem["MemTotal"]) * 100
		}
		if out := output("nvidia-smi", "--query-gpu=utilization.gpu", "--format=csv,noheader,nounits"); out != "" {
			gpu(number(strings.Split(out, "\n")[0]))
		} else if files, _ := filepath.Glob("/sys/class/drm/card*/device/gpu_busy_percent"); len(files) > 0 {
			data, _ := os.ReadFile(files[0])
			gpu(number(string(data)))
		}
	}
	s.CPU = min(100, max(0, s.CPU))
	return s
}

func watchStats() {
	for {
		stats.Lock()
		stats.samples = append(stats.samples, sample())
		if n := len(stats.samples); n > statsKeep {
			stats.samples = stats.samples[n-statsKeep:]
		}
		stats.Unlock()
		time.Sleep(statsEvery)
	}
}

func (s *Server) handleStats(*http.Request) (any, error) {
	stats.Lock()
	defer stats.Unlock()
	return append([]Sample{}, stats.samples...), nil
}
