package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"

	"github.com/BurntSushi/toml"
)

// Curve is a list of [temp °C, duty %] points, linear between them and flat
// beyond the ends.
type Curve [][2]float64

type Sensor struct {
	Name  string `toml:"name"`
	Hwmon string `toml:"hwmon"`
	Label string `toml:"label"`
	Index int    `toml:"index"`
}

type FanConfig struct {
	Name          string   `toml:"name"`
	Label         string   `toml:"label"`
	Hwmon         string   `toml:"hwmon"`
	PWM           int      `toml:"pwm"`
	Tach          int      `toml:"tach"`
	Scale         *float64 `toml:"scale"`
	Points        Curve    `toml:"points"`
	RampUp        *float64 `toml:"ramp_up"`
	RampDown      *float64 `toml:"ramp_down"`
	RestoreEnable *int     `toml:"restore_enable"`
	AvoidBeatWith string   `toml:"avoid_beat_with"`
	BeatBand      *float64 `toml:"beat_band"`
	History       bool     `toml:"history"`
}

type Preset struct {
	Min         float64 `toml:"min"`
	GamingFloor float64 `toml:"gaming_floor"`
	CPU         Curve   `toml:"cpu"`
	GPU         Curve   `toml:"gpu"`
	RampUp      float64 `toml:"ramp_up"`
	RampDown    float64 `toml:"ramp_down"`
}

type GameConfig struct {
	Preset     string  `toml:"preset"`
	GamingUtil float64 `toml:"gaming_util"`
	QuietUtil  float64 `toml:"quiet_util"`
	EnterS     float64 `toml:"enter_s"`
	ExitS      float64 `toml:"exit_s"`
}

type PowerConfig struct {
	RestW         float64 `toml:"rest_w"`
	PSUEfficiency float64 `toml:"psu_efficiency"`
	TariffPerKWh  float64 `toml:"tariff_per_kwh"`
	Currency      string  `toml:"currency"`
}

type Config struct {
	DefaultPreset string  `toml:"default_preset"`
	Failsafe      float64 `toml:"failsafe"`
	Sensors       struct {
		CPU  Sensor   `toml:"cpu"`
		GPU  any      `toml:"gpu"` // "nvml" or a sensor table
		Temp []Sensor `toml:"temp"`
	} `toml:"sensors"`
	Fans        []FanConfig       `toml:"fan"`
	Presets     map[string]Preset `toml:"presets"`
	PresetOrder []string          `toml:"-"`
	Game        GameConfig        `toml:"game"`
	Power       *PowerConfig      `toml:"power"`
}

func loadConfig() (*Config, error) {
	var c Config
	md, err := toml.DecodeFile(configFile, &c)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no config at %s; start from one in examples/", configFile)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", configFile, err)
	}
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%s: "+format, append([]any{configFile}, a...)...)
	}
	// Presets in file order, for the widget's buttons.
	for _, key := range md.Keys() {
		if len(key) == 2 && key[0] == "presets" && !slices.Contains(c.PresetOrder, key[1]) {
			c.PresetOrder = append(c.PresetOrder, key[1])
		}
	}
	if c.Sensors.CPU.Hwmon == "" {
		return nil, bad("[sensors] needs cpu = { hwmon = ..., label = ... }")
	}
	if len(c.Fans) == 0 {
		return nil, bad("no [[fan]] entries")
	}
	names := map[string]bool{}
	for i := range c.Fans {
		f := &c.Fans[i]
		if f.Name == "" || f.Hwmon == "" || f.PWM == 0 {
			return nil, bad("every [[fan]] needs name, hwmon and pwm")
		}
		names[f.Name] = true
		if f.Label == "" {
			f.Label = f.Name
		}
		if f.Tach == 0 {
			f.Tach = f.PWM
		}
		f.Scale = orDefault(f.Scale, 1.0)
		f.RampUp = orDefault(f.RampUp, 1.0)
		f.RampDown = orDefault(f.RampDown, 0.5)
		f.BeatBand = orDefault(f.BeatBand, 0.12)
		if f.Points != nil {
			if err := checkCurve(f.Points); err != nil {
				return nil, bad("fan %s points: %v", f.Name, err)
			}
		} else if len(c.Presets) == 0 {
			return nil, bad("fan %s follows a preset, but there is no [presets.*]", f.Name)
		}
	}
	for _, f := range c.Fans {
		if f.AvoidBeatWith != "" && !names[f.AvoidBeatWith] {
			return nil, bad("fan %s: avoid_beat_with names no fan", f.Name)
		}
	}
	for name, p := range c.Presets {
		if err := checkCurve(p.CPU); err != nil {
			return nil, bad("preset %s cpu: %v", name, err)
		}
		if err := checkCurve(p.GPU); err != nil {
			return nil, bad("preset %s gpu: %v", name, err)
		}
		if p.RampUp <= 0 || p.RampDown <= 0 {
			return nil, bad("preset %s needs ramp_up and ramp_down", name)
		}
	}
	if c.DefaultPreset == "" && len(c.PresetOrder) > 0 {
		c.DefaultPreset = c.PresetOrder[0]
	}
	g := &c.Game
	if g.Preset == "" {
		g.Preset = c.DefaultPreset
	}
	if len(c.Presets) > 0 {
		if _, ok := c.Presets[g.Preset]; !ok {
			return nil, bad("game preset %q is not a preset", g.Preset)
		}
	}
	setIfZero(&g.GamingUtil, 40)
	setIfZero(&g.QuietUtil, 25)
	setIfZero(&g.EnterS, 20)
	setIfZero(&g.ExitS, 60)
	setIfZero(&c.Failsafe, 60)
	if p := c.Power; p != nil && p.PSUEfficiency == 0 {
		p.PSUEfficiency = 0.9
	}
	return &c, nil
}

func checkCurve(c Curve) error {
	if len(c) == 0 {
		return errors.New("expected [[temp, duty%], ...]")
	}
	for i, p := range c {
		if p[1] < 0 || p[1] > 100 {
			return errors.New("duty must be in 0-100")
		}
		if i > 0 && p[0] < c[i-1][0] {
			return errors.New("temperatures must increase")
		}
	}
	return nil
}

func orDefault(v *float64, d float64) *float64 {
	if v == nil {
		return &d
	}
	return v
}

func setIfZero(v *float64, d float64) {
	if *v == 0 {
		*v = d
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
