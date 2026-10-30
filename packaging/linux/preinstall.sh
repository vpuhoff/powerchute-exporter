#!/bin/sh
set -eu

if ! getent group powerchute-exporter >/dev/null; then
    groupadd --system powerchute-exporter
fi
if ! getent passwd powerchute-exporter >/dev/null; then
    useradd --system --gid powerchute-exporter --home-dir /nonexistent \
        --no-create-home --shell /usr/sbin/nologin powerchute-exporter
fi
