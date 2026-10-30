package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func rd(path string) (string, error) {
	b, err := os.ReadFile(path)
	return strings.TrimSpace(string(b)), err
}

func wr(path string, v any) error {
	return os.WriteFile(path, []byte(fmt.Sprint(v)), 0)
}

func rdInt(path string) (int, error) {
	s, err := rd(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(s)
}

func hwmonDirs(name string) []string {
	files, _ := filepath.Glob("/sys/class/hwmon/hwmon*/name")
	sort.Strings(files)
	var dirs []string
	for _, f := range files {
		if n, err := rd(f); err == nil && n == name {
			dirs = append(dirs, filepath.Dir(f))
		}
	}
	return dirs
}

// hwmonTemp reads °C from the first hwmon called s.Hwmon, by label or tempN.
func hwmonTemp(s Sensor) *float64 {
	for _, d := range hwmonDirs(s.Hwmon) {
		if s.Label == "" {
			return milli(fmt.Sprintf("%s/temp%d_input", d, max(s.Index, 1)))
		}
		labels, _ := filepath.Glob(d + "/temp*_label")
		for _, lp := range labels {
			if l, _ := rd(lp); l == s.Label {
				return milli(strings.Replace(lp, "_label", "_input", 1))
			}
		}
		return nil
	}
	return nil
}

func milli(path string) *float64 {
	v, err := rdInt(path)
	if err != nil {
		return nil
	}
	t := float64(v) / 1000
	return &t
}

// Fan is one pwmN on a hwmon device, in manual mode while the daemon runs.
type Fan struct {
	cfg  FanConfig
	dir  string
	last int
}

func newFan(c FanConfig) *Fan { return &Fan{cfg: c, last: -1} }

func (f *Fan) path() string {
	if f.dir == "" || !exists(f.dir) {
		f.dir, f.last = "", -1
		if dirs := hwmonDirs(f.cfg.Hwmon); len(dirs) > 0 {
			f.dir = dirs[0]
		}
	}
	return f.dir
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func (f *Fan) pwmPath() string { return fmt.Sprintf("%s/pwm%d", f.path(), f.cfg.PWM) }

func pct2pwm(p float64) int { return max(0, min(255, int(p*255/100+0.5))) }

func (f *Fan) set(pct float64, force bool) {
	if f.path() == "" {
		return
	}
	v := pct2pwm(pct)
	pwm := f.pwmPath()
	// Firmware may put the header back in auto mode after resume.
	if mode, err := rd(pwm + "_enable"); force || err != nil || mode != "1" {
		if err := wr(pwm+"_enable", 1); err != nil {
			logf("%s: write failed: %v", f.cfg.Name, err)
			f.dir = ""
			return
		}
		f.last = -1
	}
	if force || f.last != v {
		if err := wr(pwm, v); err != nil {
			logf("%s: write failed: %v", f.cfg.Name, err)
			f.dir = ""
			return
		}
		f.last = v
	}
}

func (f *Fan) rpm() int {
	if f.path() == "" {
		return -1
	}
	v, err := rdInt(fmt.Sprintf("%s/fan%d_input", f.dir, f.cfg.Tach))
	if err != nil {
		return -1
	}
	return v
}

// restore hands the fan back to the firmware curve if configured, else holds
// the failsafe duty.
func (f *Fan) restore(failsafe float64) error {
	if f.cfg.RestoreEnable != nil {
		if f.path() == "" {
			return nil
		}
		return wr(f.pwmPath()+"_enable", *f.cfg.RestoreEnable)
	}
	f.set(failsafe, true)
	return nil
}
