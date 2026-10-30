package main

import (
	"fmt"
	"strings"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

type GPU interface {
	Temp() *float64
	Util() *float64
	Details() map[string]any
}

func makeGPU(spec any) GPU {
	switch s := spec.(type) {
	case string:
		if s == "nvml" {
			return newNVML()
		}
	case map[string]any:
		sensor := Sensor{}
		sensor.Hwmon, _ = s["hwmon"].(string)
		sensor.Label, _ = s["label"].(string)
		if i, ok := s["index"].(int64); ok {
			sensor.Index = int(i)
		}
		return hwmonGPU{sensor}
	}
	return noGPU{}
}

type noGPU struct{}

func (noGPU) Temp() *float64          { return nil }
func (noGPU) Util() *float64          { return nil }
func (noGPU) Details() map[string]any { return map[string]any{} }

// hwmonGPU is a GPU known only through hwmon: temperature, plus busy % if the
// driver exposes gpu_busy_percent (amdgpu).
type hwmonGPU struct{ s Sensor }

func (g hwmonGPU) Temp() *float64 { return hwmonTemp(g.s) }

func (g hwmonGPU) Util() *float64 {
	for _, d := range hwmonDirs(g.s.Hwmon) {
		v, err := rdInt(d + "/device/gpu_busy_percent")
		if err != nil {
			return nil
		}
		u := float64(v)
		return &u
	}
	return nil
}

func (hwmonGPU) Details() map[string]any { return map[string]any{} }

// NVML through NVIDIA's go-nvml, which loads libnvidia-ml.so at runtime.
type nvmlGPU struct {
	dev nvml.Device
	ok  bool
}

func newNVML() GPU {
	if nvml.Init() != nvml.SUCCESS {
		return noGPU{}
	}
	dev, ret := nvml.DeviceGetHandleByIndex(0)
	if ret != nvml.SUCCESS {
		return noGPU{}
	}
	return &nvmlGPU{dev: dev, ok: true}
}

func (g *nvmlGPU) Temp() *float64 {
	t, ret := g.dev.GetTemperature(nvml.TEMPERATURE_GPU)
	if ret != nvml.SUCCESS {
		return nil
	}
	v := float64(t)
	return &v
}

func (g *nvmlGPU) Util() *float64 {
	u, ret := g.dev.GetUtilizationRates()
	if ret != nvml.SUCCESS {
		return nil
	}
	v := float64(u.Gpu)
	return &v
}

func (g *nvmlGPU) Details() map[string]any {
	out := map[string]any{}
	if name, ret := g.dev.GetName(); ret == nvml.SUCCESS {
		out["name"] = strings.TrimPrefix(name, "NVIDIA GeForce ")
	}
	if p, ret := g.dev.GetPowerUsage(); ret == nvml.SUCCESS {
		out["power_w"] = round1(float64(p) / 1000)
	}
	if c, ret := g.dev.GetClockInfo(nvml.CLOCK_GRAPHICS); ret == nvml.SUCCESS {
		out["clock_mhz"] = c
	}
	if c, ret := g.dev.GetClockInfo(nvml.CLOCK_MEM); ret == nvml.SUCCESS {
		out["mem_clock_mhz"] = c
	}
	if f, ret := g.dev.GetFanSpeed(); ret == nvml.SUCCESS {
		out["fan_pct"] = f
	}
	if m, ret := g.dev.GetMemoryInfo(); ret == nvml.SUCCESS {
		out["vram_used_mb"] = m.Used >> 20
		out["vram_total_mb"] = m.Total >> 20
	}
	return out
}

func round1(v float64) float64 { return float64(int(v*10+0.5)) / 10 }

func fmtTemp(t *float64) string {
	if t == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f", *t)
}
