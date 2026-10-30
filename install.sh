#!/bin/sh
# Build fancurve and install it with its service (asks for sudo). Needs Go
# and a C compiler (NVML is loaded through cgo).
set -e
cd "$(dirname "$0")"
# NVIDIA's headers mark many functions deprecated; the build only uses current ones.
CGO_CFLAGS=-Wno-deprecated-declarations go build -trimpath -o fancurve .
owner=$(id -un)
sudo install -Dm755 fancurve /usr/local/bin/fancurve
sudo install -Dm644 fancurve.service /etc/systemd/system/fancurve.service
sudo install -dm755 /var/lib/fancurve
for f in preset game; do
  [ -f "/var/lib/fancurve/$f" ] || sudo touch "/var/lib/fancurve/$f"
  # You can switch presets and game mode without sudo.
  sudo chown "$owner" "/var/lib/fancurve/$f"
  sudo chmod 644 "/var/lib/fancurve/$f"
done
sudo systemctl daemon-reload
if [ ! -f /etc/fancurve.toml ]; then
  cat <<MSG
Installed, not started: write /etc/fancurve.toml first (start from examples/),
check it with \`fancurve check\`, then:
  sudo systemctl enable --now fancurve
MSG
  exit 0
fi
fancurve check
sudo systemctl enable fancurve.service
sudo systemctl restart fancurve.service
