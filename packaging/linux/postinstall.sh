#!/bin/sh
set -eu

# Also works when installing into an image/chroot without a running systemd.
if command -v systemctl >/dev/null 2>&1; then
    # Do not re-enable a service deliberately disabled by the administrator.
    case "${1:-}" in
        configure) if [ -z "${2:-}" ]; then systemctl --no-reload enable powerchute-exporter.service; fi ;;
        1) systemctl --no-reload enable powerchute-exporter.service ;;
    esac
    if [ -d /run/systemd/system ]; then
        systemctl daemon-reload
        # Preserve the state of an existing installation. New installations
        # stay stopped until connection settings and credentials are configured.
        systemctl try-restart powerchute-exporter.service
    fi
fi
