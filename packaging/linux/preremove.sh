#!/bin/sh
set -eu

# Debian passes remove/deconfigure; RPM passes 0 on final removal and 1 on upgrade.
case "${1:-}" in
    remove|deconfigure|0)
        if command -v systemctl >/dev/null 2>&1; then
            if [ -d /run/systemd/system ]; then
                systemctl stop powerchute-exporter.service
            fi
            systemctl --no-reload disable powerchute-exporter.service
        fi
        ;;
esac
