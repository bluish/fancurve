// fancurve drives hwmon PWM fans from CPU and GPU temperatures.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const usage = `fancurve: fan curves for hwmon PWM fans, from CPU and GPU temperatures.

Case fans follow max(CPU curve, GPU curve, gaming floor) of the active preset,
each at its own scale; fans with their own points (a CPU cooler) follow the
CPU temperature alone. Everything is smoothed and rate limited, and fans with
restore_enable go back to the firmware's control when the daemon stops.

Usage: fancurve                 run the daemon (root)
       fancurve check           validate the config and show what it would drive
       fancurve set <preset>    switch preset
       fancurve get             print current preset
       fancurve game <mode>     game mode off|on|auto (case fans on the game preset while active)
       fancurve restore         fans back to firmware control or their failsafe duty (root)
       fancurve identify        spin each configured fan alone, to map them (root)`

const (
	stateDir     = "/var/lib/fancurve"
	historyEvery = 10  // ticks between history samples
	historyLen   = 180 // 30 minutes
	smartEvery   = 10 * time.Minute
	tempAlpha    = 0.15 // EMA per second (~6 s time constant)
	utilAlpha    = 0.1
	interval     = time.Second
	resendEvery  = 30 // rewrite outputs even if unchanged (resume, replug)
	logEvery     = 30
)

var (
	configFile  = envOr("FANCURVE_CONFIG", "/etc/fancurve.toml")
	presetFile  = stateDir + "/preset"
	gameFile    = stateDir + "/game"
	energyFile  = stateDir + "/energy.json"
	statusFile  = "/run/fancurve/status.json"
	historyFile = "/run/fancurve/history.json"
)

func logf(format string, a ...any) { fmt.Printf(format+"\n", a...) }

func itoa(i int) string { return strconv.Itoa(i) }

func die(err error) {
	fmt.Fprintln(os.Stderr, "fancurve:", err)
	os.Exit(1)
}

func interp(c Curve, t float64) float64 {
	if t <= c[0][0] {
		return c[0][1]
	}
	for i := 1; i < len(c); i++ {
		if t <= c[i][0] {
			t0, d0, t1, d1 := c[i-1][0], c[i-1][1], c[i][0], c[i][1]
			return d0 + (d1-d0)*(t-t0)/(t1-t0)
		}
	}
	return c[len(c)-1][1]
}

func ramp(cur, target, up, down float64) float64 {
	if target > cur {
		return min(target, cur+up*interval.Seconds())
	}
	return max(target, cur-down*interval.Seconds())
}

func writeJSON(path string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	os.Chmod(tmp, 0o644)
	return os.Rename(tmp, path)
}

func readGame() string {
	m, _ := rd(gameFile)
	if m == "on" || m == "auto" {
		return m
	}
	return "off"
}

func readPreset(c *Config) string {
	if n, err := rd(presetFile); err == nil {
		if _, ok := c.Presets[n]; ok {
			return n
		}
	}
	return c.DefaultPreset
}

// restore puts every fan in a safe state: firmware control where
// configured, failsafe duty elsewhere.
func restore(c *Config) {
	for _, fc := range c.Fans {
		if err := newFan(fc).restore(c.Failsafe); err != nil {
			logf("%s: restore failed: %v", fc.Name, err)
		}
	}
	os.Remove(statusFile)
}

func identify(c *Config) {
	var fans []*Fan
	for _, fc := range c.Fans {
		fans = append(fans, newFan(fc))
	}
	defer restore(c)
	for _, fan := range fans {
		for _, other := range fans {
			duty := 0.0
			if other == fan {
				duty = 100
			}
			other.set(duty, true)
		}
		logf("%s (%s pwm%d) -> 100%%, others stopped, 8s...", fan.cfg.Name, fan.cfg.Hwmon, fan.cfg.PWM)
		time.Sleep(8 * time.Second)
		for _, other := range fans {
			other.set(0, true)
		}
		time.Sleep(4 * time.Second)
	}
}

// check prints what the config resolves to, without writing anything.
func check(c *Config) {
	fmt.Println("config:", configFile)
	fmt.Println("cpu temp:", fmtTemp(hwmonTemp(c.Sensors.CPU)))
	gpu := makeGPU(c.Sensors.GPU)
	fmt.Printf("gpu temp: %s  util: %s\n", fmtTemp(gpu.Temp()), fmtTemp(gpu.Util()))
	for _, t := range extraTemps(c.Sensors.Temp) {
		v, _ := t["temp"].(*float64)
		fmt.Printf("%s: %s\n", t["name"], fmtTemp(v))
	}
	ok := true
	for _, fc := range c.Fans {
		f := newFan(fc)
		where := "no hwmon named " + fc.Hwmon + " (missing)"
		if f.path() != "" {
			where = f.pwmPath()
			if !exists(where) {
				where += " (missing)"
				ok = false
			}
		} else {
			ok = false
		}
		how := fmt.Sprintf("case x%g", *fc.Scale)
		if fc.Points != nil {
			how = "own points"
		}
		fmt.Printf("fan %s: %s  rpm %d  [%s]\n", fc.Name, where, f.rpm(), how)
	}
	fmt.Printf("presets: %s  default: %s\n", strings.Join(c.PresetOrder, ", "), c.DefaultPreset)
	if !ok {
		os.Exit(1)
	}
}

func main() {
	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}
	if cmd == "game" && len(args) == 2 {
		if !slices.Contains([]string{"off", "on", "auto"}, args[1]) {
			die(fmt.Errorf("game mode: off|on|auto"))
		}
		if err := wr(gameFile, args[1]+"\n"); err != nil {
			die(err)
		}
		return
	}
	c, err := loadConfig()
	if err != nil {
		die(err)
	}
	switch {
	case cmd == "":
		run(c)
	case cmd == "check":
		check(c)
	case cmd == "identify":
		identify(c)
	case cmd == "restore":
		restore(c)
	case cmd == "set" && len(args) == 2:
		if _, ok := c.Presets[args[1]]; !ok {
			die(fmt.Errorf("unknown preset: %s (use: %s)", args[1], strings.Join(c.PresetOrder, ", ")))
		}
		if err := wr(presetFile, args[1]+"\n"); err != nil {
			die(err)
		}
	case cmd == "get":
		fmt.Println(readPreset(c))
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
}

func run(c *Config) {
	var fans []*Fan
	for _, fc := range c.Fans {
		fans = append(fans, newFan(fc))
	}
	gpu := makeGPU(c.Sensors.GPU)
	system := newSystem(c, gpu)
	os.MkdirAll(filepath.Dir(statusFile), 0o755)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sig
		restore(c)
		os.Exit(0)
	}()
	defer func() {
		if r := recover(); r != nil {
			restore(c)
			panic(r)
		}
	}()

	cpuTemp := func() float64 {
		t := hwmonTemp(c.Sensors.CPU)
		if t == nil {
			panic("CPU temperature unavailable")
		}
		return *t
	}
	cpuS := cpuTemp()
	gpuS := 40.0
	if t := gpu.Temp(); t != nil {
		gpuS = *t
	}
	utilS := 0.0
	caseDuty, caseOut := 40.0, 40.0
	own := map[string]float64{}
	for _, f := range fans {
		if f.cfg.Points != nil {
			own[f.cfg.Name] = interp(f.cfg.Points, cpuS)
		}
	}
	beatSide := map[string]int{} // fan -> +1 / -1 while pinned to a beat band edge
	name := ""
	var preset *Preset
	tick := 0
	var history []map[string]any
	gameActive := false
	var gameSince time.Time // when the current enter/exit condition started

	for {
		gameMode := readGame()
		newName := readPreset(c)
		if gameActive {
			newName = c.Game.Preset
		}
		if newName != name {
			name = newName
			if p, ok := c.Presets[name]; ok {
				preset = &p
			}
			logf("preset: %s", name)
			tick = 0 // resend right away
		}

		cpuS += tempAlpha * (cpuTemp() - cpuS)
		g := gpu.Temp()
		if g != nil {
			gpuS += tempAlpha * (*g - gpuS)
		}
		u := gpu.Util()
		if u != nil {
			utilS += utilAlpha * (*u - utilS)
		}
		gaming := utilS >= c.Game.GamingUtil

		switch gameMode {
		case "on":
			gameActive, gameSince = true, time.Time{}
		case "off":
			gameActive, gameSince = false, time.Time{}
		default:
			// Hysteresis: sustained load to enter, sustained quiet to leave.
			flipping := gaming
			wait := c.Game.EnterS
			if gameActive {
				flipping, wait = utilS < c.Game.QuietUtil, c.Game.ExitS
			}
			switch {
			case !flipping:
				gameSince = time.Time{}
			case gameSince.IsZero():
				gameSince = time.Now()
			case time.Since(gameSince).Seconds() >= wait:
				gameActive, gameSince = !gameActive, time.Time{}
				logf("game mode auto: %v", map[bool]string{true: "on", false: "off"}[gameActive])
			}
		}

		force := tick%resendEvery == 0

		if preset != nil {
			floor := 0.0
			if gaming {
				floor = preset.GamingFloor
			}
			target := max(interp(preset.CPU, cpuS), interp(preset.GPU, gpuS), floor)
			caseDuty = ramp(caseDuty, target, preset.RampUp, preset.RampDown)
			caseOut = max(preset.Min, caseDuty)
		}
		duties := map[string]float64{}
		for _, f := range fans {
			fc := f.cfg
			if fc.Points != nil {
				own[fc.Name] = ramp(own[fc.Name], interp(fc.Points, cpuS), *fc.RampUp, *fc.RampDown)
				duties[fc.Name] = own[fc.Name]
			} else {
				duties[fc.Name] = caseOut * *fc.Scale
			}
		}
		// Same-model fans at nearly the same duty beat against each other.
		// Pin the fan to the band edge it entered from; it only crosses once
		// its own target has cleared the whole band.
		for _, f := range fans {
			fc := f.cfg
			if fc.AvoidBeatWith == "" {
				continue
			}
			ref, d := duties[fc.AvoidBeatWith], duties[fc.Name]
			lo, hi := ref*(1-*fc.BeatBand), ref*(1+*fc.BeatBand)
			if lo < d && d < hi {
				side, ok := beatSide[fc.Name]
				if !ok {
					side = -1
					if d >= ref {
						side = 1
					}
					beatSide[fc.Name] = side
				}
				duties[fc.Name] = map[bool]float64{true: hi, false: lo}[side > 0]
			} else {
				delete(beatSide, fc.Name)
			}
		}
		for _, f := range fans {
			f.set(duties[f.cfg.Name], force)
		}

		fanStatus := []map[string]any{}
		for _, f := range fans {
			fanStatus = append(fanStatus, map[string]any{
				"name": f.cfg.Name, "label": f.cfg.Label, "pct": int(duties[f.cfg.Name] + 0.5),
				"rpm": f.rpm(), "history": f.cfg.History,
			})
		}
		status := map[string]any{
			"time": float64(time.Now().UnixMilli()) / 1000, "preset": readPreset(c), "effective_preset": name,
			"presets": c.PresetOrder, "gaming": gaming,
			"game": map[string]any{"mode": gameMode, "active": gameActive},
			"cpu":  round1(cpuS), "gpu": nil, "gpu_util": nil, "fans": fanStatus,
		}
		if g != nil {
			status["gpu"] = round1(gpuS)
		}
		if u != nil {
			status["gpu_util"] = int(utilS + 0.5)
		}
		status["system"] = safeCollect(system)
		if writeJSON(statusFile, status) == nil && tick%historyEvery == 0 {
			history = append(history, historySample(status))
			if len(history) > historyLen {
				history = history[len(history)-historyLen:]
			}
			writeJSON(historyFile, map[string]any{"every": historyEvery, "samples": history})
		}

		if tick%logEvery == 0 {
			var parts []string
			for _, s := range fanStatus {
				parts = append(parts, fmt.Sprintf("%s %d%% %d", s["name"], s["pct"], s["rpm"]))
			}
			extra := ""
			if gaming {
				extra = " GAMING"
			}
			logf("[%s] cpu %.1f gpu %.1f util %.0f%%%s | %s", name, cpuS, gpuS, utilS, extra, strings.Join(parts, " "))
		}
		tick++
		time.Sleep(interval)
	}
}

// safeCollect keeps sensors from ever stopping fan control.
func safeCollect(s *System) (out map[string]any) {
	defer func() {
		if r := recover(); r != nil {
			logf("system collect failed: %v", r)
			out = nil
		}
	}()
	return s.collect()
}

// historySample is the handful of values the sensors panel charts.
func historySample(status map[string]any) map[string]any {
	sample := map[string]any{"t": int64(status["time"].(float64)), "cpu": status["cpu"]}
	if sys, ok := status["system"].(map[string]any); ok {
		if t, ok := sys["cpu"].(map[string]any)["temp"].(*float64); ok && t != nil {
			sample["cpu"] = round1(*t)
		}
		sample["gpu"] = sys["gpu"].(map[string]any)["temp"]
		if p, ok := sys["power"].(map[string]any); ok {
			sample["power"] = p["wall_w"]
		}
	}
	fans := map[string]any{}
	for _, f := range status["fans"].([]map[string]any) {
		if f["history"].(bool) {
			fans[f["name"].(string)] = f["rpm"]
		}
	}
	sample["fans"] = fans
	return sample
}
