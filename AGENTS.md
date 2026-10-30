# AGENTS.md

`fancurve` is a Go daemon that runs as root and writes to fan PWM files in
sysfs. Dependencies: BurntSushi/toml (config) and NVIDIA/go-nvml (loads
`libnvidia-ml.so` at runtime; needs cgo to build). Hardware never appears in the code or the docs (only in `examples/`): it all comes from
`/etc/fancurve.toml` (`FANCURVE_CONFIG` overrides the path).

## Layout

- `config.go`: TOML config and its validation.
- `hwmon.go`: sysfs helpers, temperature sensors, `Fan`.
- `gpu.go`: GPU backends (NVML, hwmon, none).
- `system.go`: the status file's `system` section (power, NVMe, disks).
- `main.go`: CLI, the control loop, status and history.
- `fancurve.service`: systemd unit; `ExecStopPost=fancurve restore` is the
  safety net and must stay.
- `examples/`: complete configs.

## Rules

- A fan that is wrongly stopped can cook hardware. Any change to the control
  loop keeps: failsafe on exit and on exceptions, ramps on every duty,
  `restore` handing headers back to the firmware.
- Sensors must never stop fan control: `System.collect` runs under
  `safeCollect`, and a missing optional sensor reads as nil.
- `status.json` and `history.json` are read by bar widgets; keep their
  shape or say so in the release notes.
- No dependencies beyond the two above.

## Testing without touching the fans

- `go vet ./...`, then `FANCURVE_CONFIG=examples/<file>.toml ./fancurve
  check` resolves the config against this machine's sysfs and writes nothing.
- For the loop, in a temp copy: make `wr` record instead of write, point
  `statusFile`/`historyFile`/`energyFile` at a temp dir, drop the
  `time.Sleep` and exit after ~120 ticks. Duties should converge to what the
  curves give for the current temperatures (compare with a running daemon).
- Only run the real daemon (`sudo systemctl restart fancurve`) on a machine
  whose config you know, and watch `journalctl -u fancurve -f`.

## Commits

`type(scope): description` in English; body with why, then lowercase
bullets of what changed, then a `Validation:` line with what was actually run.
