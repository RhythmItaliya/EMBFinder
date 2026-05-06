package main

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type PerfMode string

const (
	PerfModeAuto        PerfMode = "auto"
	PerfModePerformance PerfMode = "performance"
	PerfModeBalanced    PerfMode = "balanced"
	PerfModePowerSaver  PerfMode = "power_saver"
)

type PerfMetrics struct {
	TimestampUnix int64   `json:"timestamp_unix"`
	Mode          string  `json:"mode"`
	EffectiveMode string  `json:"effective_mode"`
	CPUCores      int     `json:"cpu_cores"`
	CPUUsagePct   float64 `json:"cpu_usage_pct"`
	Load1         float64 `json:"load_1"`
	RAMTotalGB    int     `json:"ram_total_gb"`
	RAMAvailGB    float64 `json:"ram_avail_gb"`
	GPUModel      string  `json:"gpu_model"`
	VRAMMB        int     `json:"vram_mb"`
	DiskType      string  `json:"disk_type"`
	TempC         float64 `json:"temp_c"`
	IndexWorkers  int     `json:"index_workers"`
	SearchWorkers int     `json:"search_workers"`
}

type perfState struct {
	mode          atomic.Value
	metrics       atomic.Value
	lastCPU       cpuStat
	lastCPUMu     sync.Mutex
	indexWorkers  atomic.Int32
	searchWorkers atomic.Int32
}

var perf = &perfState{}

func initPerfManager() {
	m := parsePerfMode(os.Getenv("PERF_MODE"))
	perf.mode.Store(m)
	perf.indexWorkers.Store(int32(Config.IndexWorkers))
	perf.searchWorkers.Store(int32(Config.SearchWorkers))

	go func() {
		for {
			updatePerf()
			time.Sleep(5 * time.Second)
		}
	}()
}

func parsePerfMode(v string) PerfMode {
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case string(PerfModePerformance):
		return PerfModePerformance
	case string(PerfModePowerSaver):
		return PerfModePowerSaver
	case string(PerfModeBalanced):
		return PerfModeBalanced
	case string(PerfModeAuto):
		return PerfModeAuto
	default:
		return PerfModeAuto
	}
}

func setPerfMode(m PerfMode) {
	perf.mode.Store(m)
}

func getPerfMode() PerfMode {
	v := perf.mode.Load()
	if v == nil {
		return PerfModeAuto
	}
	return v.(PerfMode)
}

func getPerfMetrics() PerfMetrics {
	v := perf.metrics.Load()
	if v == nil {
		return PerfMetrics{}
	}
	return v.(PerfMetrics)
}

func getIndexWorkers() int {
	v := perf.indexWorkers.Load()
	if v < 1 {
		return Config.IndexWorkers
	}
	return int(v)
}

func getSearchWorkers() int {
	v := perf.searchWorkers.Load()
	if v < 1 {
		return Config.SearchWorkers
	}
	return int(v)
}

type cpuStat struct {
	idle  uint64
	total uint64
}

func updatePerf() {
	cpuUsage := readCPUUsage()
	load1 := readLoad1()
	ramAvailGB := readMemAvailableGB()
	tempC := readMaxTempC()
	gpuModel, vramMB := readGPUInfo()
	diskType := detectDiskType(Config.DBPath)

	effectiveMode := computeEffectiveMode(cpuUsage, ramAvailGB, tempC)
	indexWorkers, searchWorkers := computeWorkerTargets(effectiveMode, cpuUsage, ramAvailGB, tempC)

	perf.indexWorkers.Store(int32(indexWorkers))
	perf.searchWorkers.Store(int32(searchWorkers))

	if ramAvailGB > 0 && ramAvailGB < 1.5 {
		MemoryCleanup()
	}

	metrics := PerfMetrics{
		TimestampUnix: time.Now().Unix(),
		Mode:          string(getPerfMode()),
		EffectiveMode: string(effectiveMode),
		CPUCores:      Config.CPUCores,
		CPUUsagePct:   cpuUsage,
		Load1:         load1,
		RAMTotalGB:    Config.TotalRAMGB,
		RAMAvailGB:    ramAvailGB,
		GPUModel:      gpuModel,
		VRAMMB:        vramMB,
		DiskType:      diskType,
		TempC:         tempC,
		IndexWorkers:  indexWorkers,
		SearchWorkers: searchWorkers,
	}
	perf.metrics.Store(metrics)
}

func computeEffectiveMode(cpuUsage, ramAvailGB, tempC float64) PerfMode {
	mode := getPerfMode()
	if mode != PerfModeAuto {
		return mode
	}

	if tempC > 80 || (ramAvailGB > 0 && ramAvailGB < 2) {
		return PerfModePowerSaver
	}
	if Config.TotalRAMGB >= 24 && Config.CPUCores >= 8 && cpuUsage < 60 {
		return PerfModePerformance
	}
	return PerfModeBalanced
}

func computeWorkerTargets(mode PerfMode, cpuUsage, ramAvailGB, tempC float64) (int, int) {
	baseIndex := Config.IndexWorkers
	baseSearch := Config.SearchWorkers

	switch mode {
	case PerfModePerformance:
		baseIndex = clamp(baseIndex+1, 2, Config.CPUCores)
		baseSearch = clamp(baseSearch+1, 1, Config.CPUCores)
	case PerfModePowerSaver:
		baseIndex = clamp(1, 1, Config.CPUCores)
		baseSearch = clamp(1, 1, Config.CPUCores)
	case PerfModeBalanced:
		// keep base
	}

	if cpuUsage > 90 || tempC > 85 {
		baseIndex = clamp(baseIndex/2, 1, Config.CPUCores)
		baseSearch = clamp(baseSearch/2, 1, Config.CPUCores)
	}
	if ramAvailGB > 0 && ramAvailGB < 2 {
		baseIndex = 1
		baseSearch = 1
	}
	return baseIndex, baseSearch
}

func clamp(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func readCPUUsage() float64 {
	if runtime.GOOS != "linux" {
		return 0
	}
	stat, err := readCPUStat()
	if err != nil {
		return 0
	}
	perf.lastCPUMu.Lock()
	defer perf.lastCPUMu.Unlock()
	if perf.lastCPU.total == 0 {
		perf.lastCPU = stat
		return 0
	}
	deltaTotal := float64(stat.total - perf.lastCPU.total)
	deltaIdle := float64(stat.idle - perf.lastCPU.idle)
	perf.lastCPU = stat
	if deltaTotal <= 0 {
		return 0
	}
	usage := (deltaTotal - deltaIdle) / deltaTotal * 100
	if usage < 0 {
		return 0
	}
	return usage
}

func readCPUStat() (cpuStat, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return cpuStat{}, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "cpu ") {
			fields := strings.Fields(line)
			if len(fields) < 5 {
				return cpuStat{}, errors.New("cpu stat fields")
			}
			var total uint64
			for i := 1; i < len(fields); i++ {
				v, _ := strconv.ParseUint(fields[i], 10, 64)
				total += v
			}
			idle, _ := strconv.ParseUint(fields[4], 10, 64)
			return cpuStat{idle: idle, total: total}, nil
		}
	}
	return cpuStat{}, errors.New("cpu line not found")
}

func readLoad1() float64 {
	if runtime.GOOS != "linux" {
		return 0
	}
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(fields[0], 64)
	return v
}

func readMemAvailableGB() float64 {
	if runtime.GOOS != "linux" {
		return 0
	}
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "MemAvailable:") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return 0
			}
			kb, err := strconv.ParseFloat(fields[1], 64)
			if err != nil {
				return 0
			}
			return kb / 1024 / 1024
		}
	}
	return 0
}

func readMaxTempC() float64 {
	if runtime.GOOS != "linux" {
		return 0
	}
	zones, err := os.ReadDir("/sys/class/thermal")
	if err != nil {
		return 0
	}
	max := 0.0
	for _, z := range zones {
		if !strings.HasPrefix(z.Name(), "thermal_zone") {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/sys/class/thermal", z.Name(), "temp"))
		if err != nil {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
		if err != nil {
			continue
		}
		c := v / 1000.0
		if c > max {
			max = c
		}
	}
	return max
}

func readGPUInfo() (string, int) {
	cmd := exec.Command("nvidia-smi", "--query-gpu=name,memory.total", "--format=csv,noheader,nounits")
	out, err := cmd.Output()
	if err != nil {
		return "unknown", 0
	}
	line := strings.TrimSpace(string(out))
	parts := strings.Split(line, ",")
	if len(parts) < 2 {
		return strings.TrimSpace(line), 0
	}
	name := strings.TrimSpace(parts[0])
	vram, _ := strconv.Atoi(strings.TrimSpace(parts[1]))
	return name, vram
}

func detectDiskType(path string) string {
	if runtime.GOOS != "linux" {
		return "unknown"
	}
	dirs, err := os.ReadDir("/sys/block")
	if err != nil {
		return "unknown"
	}
	foundSSD := false
	for _, d := range dirs {
		rotationalPath := filepath.Join("/sys/block", d.Name(), "queue/rotational")
		b, err := os.ReadFile(rotationalPath)
		if err != nil {
			continue
		}
		rot := strings.TrimSpace(string(b))
		if rot == "0" {
			foundSSD = true
			if strings.HasPrefix(d.Name(), "nvme") {
				return "nvme"
			}
		}
	}
	if foundSSD {
		return "ssd"
	}
	return "hdd"
}
