# fancurve

A fan curve daemon for Linux (Go, one binary) that drives any fan
the kernel exposes as a hwmon `pwmN`: motherboard headers (`nct6775`,
`it87`, ...), USB fan controllers with a hwmon driver, anything with a
writable PWM.

- **Case fans** follow the maximum of a CPU curve, a GPU curve and a "gaming
  floor" while the GPU is busy, each fan at its own scale. Temperatures go
  through an EMA and duties are rate limited, so a one-second spike (common
  on Ryzen X3D chips) doesn't rev the fans.
- **Fans with their own curve** (a CPU cooler) follow the CPU temperature
  alone.
- **Anti-beat**: same-model fans at nearly the same speed beat against each
  other. Give them different scales, and `avoid_beat_with` keeps a fan
  outside a band around another fan's duty.
- **Presets** with any names, plus a **game mode** (`off` / `on` / `auto`)
  that runs the case fans on a chosen preset while the GPU is under load.
- **Hands control back** to the firmware curve (`restore_enable`) or holds a
  failsafe duty whenever it stops, including after a crash: systemd runs
  `fancurve restore` after the service exits.
- **Status** at `/run/fancurve/status.json` (fans, temperatures, GPU, NVMe
  SMART, disks, a wall-power and cost estimate) and **history** at
  `/run/fancurve/history.json` (30 minutes, every 10 s), for bars and
  widgets.

GPU temperature and load come from NVML (NVIDIA) or any hwmon sensor
(`amdgpu` load is read from `gpu_busy_percent`).

```sh
fancurve check                              # what the config resolves to; writes nothing
fancurve set <preset>; fancurve get         # as your user
fancurve game off|on|auto                   # as your user
sudo fancurve identify    # spin each configured fan alone, to map channels
sudo fancurve restore     # firmware control / failsafe duty
```

## Configure

Everything about the hardware is in `/etc/fancurve.toml`
(`FANCURVE_CONFIG` overrides it). [`examples/`](examples) has a complete
config that comments every option:

```toml
[sensors]
cpu = { hwmon = "k10temp", label = "Tctl" }    # coretemp: { hwmon = "coretemp", label = "Package id 0" }
gpu = "nvml"

[[fan]]
name = "front"
hwmon = "nct6775"      # the hwmon `name` file
pwm = 3                # pwm3 / fan3_input

[[fan]]
name = "cooler"
hwmon = "nct6775"
pwm = 2
points = [[40, 36], [70, 57], [88, 100]]
restore_enable = 5     # pwm2_enable value that gives the header back to the BIOS

[presets.balanced]
min = 30
gaming_floor = 40
cpu = [[60, 30], [75, 40], [85, 60], [90, 80]]
gpu = [[45, 30], [60, 35], [68, 50], [75, 70], [82, 100]]
ramp_up = 2.0
ramp_down = 0.5
```

Find hwmon names with `cat /sys/class/hwmon/hwmon*/name` and temperature
labels with `sensors`. Read `pwmN_enable` before the daemon first runs to
know which value to put in `restore_enable`.

**Why manual mode instead of the chip's automatic curve:** some boards accept new auto-curve points, read them back, and keep
following the BIOS curve anyway. Manual mode always works.

## Install

```sh
sh install.sh            # builds with Go (and cgo, for NVML), installs with sudo
sudoedit /etc/fancurve.toml
fancurve check
sudo systemctl enable --now fancurve
```

The installer makes `/var/lib/fancurve/{preset,game}` writable by the
installing user, so a bar widget can switch presets without sudo. Motherboard
Super I/O chips usually need a module loaded at boot (`nct6775`, `it87`).

## Examples

- [`b650e-smart-device-v2.toml`](examples/b650e-smart-device-v2.toml): a
  Super I/O chip for the CPU cooler, a USB fan controller for the case fans,
  NVML for an NVIDIA GPU, power and cost estimates.

The `amdgpu` and `coretemp` paths are untested.

## Disclaimer

No warranty. A wrong config can stop a fan: run `fancurve check` and watch
the first minutes of `journalctl -u fancurve -f`.

## License

MIT
