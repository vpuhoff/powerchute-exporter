#!/bin/sh
set -eu

case "${1:-}" in
    remove|purge|0)
        if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
            systemctl daemon-reload
            systemctl reset-failed powerchute-exporter.service >/dev/null 2>&1 || true
        fi
        ;;
esac
# Keep the system account: local config/CA files may still be owned by its group.
