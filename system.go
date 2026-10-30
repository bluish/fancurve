package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

var diskFSTypes = map[string]bool{"ext4": true, "btrfs": true, "xfs": true, "f2fs": true, "bcachefs": true, "zfs": true}

// mountedDisks returns one mount point per local block device (btrfs
// subvolumes share one).
func mountedDisks() []string {
	raw, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || !diskFSTypes[f[2]] || !strings.HasPrefix(f[0], "/dev/") || seen[f[0]] {
			continue
		}
		seen[f[0]] = true
		out = append(out, strings.ReplaceAll(f[1], `\040`, " "))
	}
	return out
}

// extraTemps reads the configured [[sensors.temp]]. A sensor without a label
// or index reads temp1 of every hwmon with that name (one per RAM stick, ...).
func extraTemps(specs []Sensor) []map[string]any {
	out := []map[string]any{}
	for _, s := range specs {
		if s.Label != "" || s.Index != 0 {
			out = append(out, map[string]any{"name": s.Name, "temp": hwmonTemp(s)})
			continue
		}
		dirs := hwmonDirs(s.Hwmon)
		for i, d := range dirs {
			name := s.Name
			if len(dirs) > 1 {
				name = s.Name + " " + itoa(i+1)
			}
			out = append(out, map[string]any{"name": name, "temp": milli(d + "/temp1_input")})
		}
	}
	return out
}

type energy struct {
	Day     string  `json:"day"`
	DayWh   float64 `json:"day_wh"`
	Month   string  `json:"month"`
	MonthWh float64 `json:"month_wh"`
}

// System collects everything the sensors panel shows that is not fan control.
type System struct {
	cfg     *Config
	gpu     GPU
	raplUJ  int
	raplAt  time.Time
	cpuW    *float64
	smart   map[string]map[string]any
	smartAt time.Time
	energy  energy
	savedAt time.Time
}

func newSystem(cfg *Config, gpu GPU) *System {
	s := &System{cfg: cfg, gpu: gpu, raplUJ: -1, savedAt: time.Now()}
	if raw, err := os.ReadFile(energyFile); err == nil {
		json.Unmarshal(raw, &s.energy)
	}
	return s
}

func (s *System) cpuPower() *float64 {
	uj, err := rdInt("/sys/class/powercap/intel-rapl:0/energy_uj")
	if err != nil {
		return nil
	}
	wrap, err := rdInt("/sys/class/powercap/intel-rapl:0/max_energy_range_uj")
	if err != nil {
		return nil
	}
	now := time.Now()
	lastUJ, lastAt := s.raplUJ, s.raplAt
	s.raplUJ, s.raplAt = uj, now
	if lastUJ < 0 {
		return nil
	}
	w := float64((uj-lastUJ+wrap)%wrap) / 1e6 / now.Sub(lastAt).Seconds()
	return &w
}

func (s *System) readSmart() map[string]map[string]any {
	out := map[string]map[string]any{}
	devs, _ := filepath.Glob("/dev/nvme[0-9]")
	for _, dev := range devs {
		raw, err := exec.Command("nvme", "smart-log", dev, "-o", "json").Output()
		var j map[string]any
		if err != nil || json.Unmarshal(raw, &j) != nil {
			continue
		}
		if _, ok := j["avail_spare"]; !ok {
			continue // error object (no permission, unsupported drive)
		}
		used := j["percent_used"]
		if used == nil {
			used = j["percentage_used"]
		}
		written, _ := j["data_units_written"].(float64)
		out[filepath.Base(dev)] = map[string]any{
			"used_pct": used, "spare_pct": j["avail_spare"], "warning": j["critical_warning"],
			"written_tb": round1(written * 512000 / 1e12), "hours": j["power_on_hours"],
			"media_errors": j["media_errors"],
		}
	}
	return out
}

func (s *System) nvme() []map[string]any {
	drives := []map[string]any{}
	ctrls, _ := filepath.Glob("/sys/class/nvme/nvme[0-9]")
	sort.Strings(ctrls)
	for _, n := range ctrls {
		name := filepath.Base(n)
		model, _ := rd(n + "/model")
		d := map[string]any{"dev": name, "model": model, "temp": nil}
		if inputs, _ := filepath.Glob(n + "/hwmon*/temp1_input"); len(inputs) > 0 {
			d["temp"] = milli(inputs[len(inputs)-1])
		}
		for k, v := range s.smart[name] {
			d[k] = v
		}
		drives = append(drives, d)
	}
	return drives
}

func (s *System) account(wallW float64) {
	now := time.Now()
	day, month := now.Format("2006-01-02"), now.Format("2006-01")
	e := &s.energy
	if e.Day != day {
		e.Day, e.DayWh = day, 0
	}
	if e.Month != month {
		e.Month, e.MonthWh = month, 0
	}
	e.DayWh += wallW * interval.Seconds() / 3600
	e.MonthWh += wallW * interval.Seconds() / 3600
	if time.Since(s.savedAt) >= time.Minute {
		if raw, err := json.Marshal(e); err == nil && os.WriteFile(energyFile, raw, 0o644) == nil {
			s.savedAt = time.Now()
		}
	}
}

// power estimates wall power: CPU package (RAPL) + GPU board power + rest_w
// for everything else, over the PSU efficiency.
func (s *System) power(gpu map[string]any) map[string]any {
	p := s.cfg.Power
	gpuW, _ := gpu["power_w"].(float64)
	cpuW := 0.0
	if s.cpuW != nil {
		cpuW = *s.cpuW
	}
	wallW := (cpuW + gpuW + p.RestW) / p.PSUEfficiency
	s.account(wallW)
	r2 := func(v float64) float64 { return float64(int(v*100+0.5)) / 100 }
	return map[string]any{
		"wall_w": int(wallW + 0.5), "rest_w": p.RestW,
		"cost_per_hour": r2(wallW / 1000 * p.TariffPerKWh),
		"today_kwh":     r2(s.energy.DayWh / 1000),
		"month_kwh":     round1(s.energy.MonthWh / 1000),
		"today_cost":    r2(s.energy.DayWh / 1000 * p.TariffPerKWh),
		"month_cost":    r2(s.energy.MonthWh / 1000 * p.TariffPerKWh),
		"tariff":        p.TariffPerKWh, "currency": p.Currency, "psu_efficiency": p.PSUEfficiency,
	}
}

func (s *System) collect() map[string]any {
	if w := s.cpuPower(); w != nil {
		s.cpuW = w
	}
	if time.Since(s.smartAt) >= smartEvery {
		s.smart, s.smartAt = s.readSmart(), time.Now()
	}
	gpu := s.gpu.Details()
	gpu["temp"] = s.gpu.Temp()
	gpu["util"] = s.gpu.Util()
	disks := []map[string]any{}
	for _, m := range mountedDisks() {
		var st syscall.Statfs_t
		if syscall.Statfs(m, &st) != nil {
			continue
		}
		disks = append(disks, map[string]any{"mount": m,
			"total_gb": int(float64(st.Blocks*uint64(st.Frsize))/1e9 + 0.5),
			"free_gb":  int(float64(st.Bavail*uint64(st.Frsize))/1e9 + 0.5)})
	}
	cpu := map[string]any{"temp": hwmonTemp(s.cfg.Sensors.CPU), "power_w": nil}
	if s.cpuW != nil {
		cpu["power_w"] = round1(*s.cpuW)
	}
	out := map[string]any{
		"cpu": cpu, "gpu": gpu, "temps": extraTemps(s.cfg.Sensors.Temp),
		"nvme": s.nvme(), "disks": disks,
	}
	if s.cfg.Power != nil {
		out["power"] = s.power(gpu)
	}
	return out
}
