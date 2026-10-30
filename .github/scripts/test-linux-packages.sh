#!/usr/bin/env bash
set -euo pipefail

# Test a genuinely changed package configuration at a higher package release.
mkdir -p dist/package-test
cp configs/config.example.yaml dist/package-test/config.yaml
printf '\n# New package configuration revision.\n' >> dist/package-test/config.yaml
sed -e "s/release: '1'/release: '2'/" \
    -e 's@src: configs/config.example.yaml@src: dist/package-test/config.yaml@' \
    packaging/nfpm.yaml > dist/package-test/nfpm.yaml
nfpm package --config dist/package-test/nfpm.yaml --packager deb --target dist/package-test/upgrade.deb
nfpm package --config dist/package-test/nfpm.yaml --packager rpm --target dist/package-test/upgrade.rpm

for distro in debian:bookworm-slim rockylinux:9; do
    docker run --rm --interactive --volume "$PWD:/src:ro" "$distro" bash -seu <<'TEST'
if command -v apt-get >/dev/null; then
    apt-get update -qq
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq ca-certificates passwd systemd curl findutils util-linux diffutils
    package=$(find /src/dist -maxdepth 1 -name '*_linux_amd64.deb' -print -quit)
    dpkg -i "$package"
else
    dnf install -y ca-certificates shadow-utils systemd curl-minimal findutils util-linux diffutils
    package=$(find /src/dist -maxdepth 1 -name '*_linux_amd64.rpm' -print -quit)
    rpm -ivh "$package"
fi

test "$(stat -c '%a' /etc/powerchute-exporter/config.yaml)" = 640
test "$(stat -c '%G' /etc/powerchute-exporter/config.yaml)" = powerchute-exporter
test "$(stat -c '%a' /etc/powerchute-exporter)" = 750
getent passwd powerchute-exporter
systemctl is-enabled powerchute-exporter.service
systemd-analyze verify /usr/lib/systemd/system/powerchute-exporter.service

cat > /etc/powerchute-exporter/config.yaml <<'YAML'
powerchute:
  base_url: http://127.0.0.1:1
  timezone: UTC
  username: package-test-user
  password: package-test-password
energy:
  currency: USD
datalog:
  enabled: false
YAML
cp /etc/powerchute-exporter/config.yaml /tmp/expected-config.yaml
runuser -u powerchute-exporter -- /usr/bin/powerchute-exporter -config /etc/powerchute-exporter/config.yaml >/tmp/exporter.log 2>&1 &
pid=$!
trap 'kill -TERM "$pid" 2>/dev/null || true' EXIT
for attempt in $(seq 1 30); do
    if curl -fsS http://127.0.0.1:9787/metrics >/tmp/metrics; then break; fi
    sleep 1
done
grep -q powerchute_exporter_build_info /tmp/metrics
kill -TERM "$pid"
wait "$pid" || true
trap - EXIT

# Respect an administrator who disabled automatic startup before the upgrade.
systemctl --no-reload disable powerchute-exporter.service
if command -v dpkg >/dev/null; then
    dpkg --force-confold -i /src/dist/package-test/upgrade.deb
else
    rpm -Uvh /src/dist/package-test/upgrade.rpm
fi
cmp /tmp/expected-config.yaml /etc/powerchute-exporter/config.yaml
test "$(systemctl is-enabled powerchute-exporter.service || true)" = disabled
if command -v dpkg >/dev/null; then
    dpkg -r powerchute-exporter
else
    rpm -e powerchute-exporter
fi
test ! -f /usr/bin/powerchute-exporter
test ! -e /etc/systemd/system/multi-user.target.wants/powerchute-exporter.service
if [ -e /etc/powerchute-exporter/config.yaml ]; then
    cmp /tmp/expected-config.yaml /etc/powerchute-exporter/config.yaml
else
    cmp /tmp/expected-config.yaml /etc/powerchute-exporter/config.yaml.rpmsave
fi
TEST
done
