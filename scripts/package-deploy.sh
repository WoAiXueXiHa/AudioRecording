#!/usr/bin/env bash
set -euo pipefail
cd -- "$(dirname -- "$0")/.."
output=$(mktemp -d /tmp/audio-deploy-XXXXXXXX)
docker build --platform linux/amd64 -t audio-recording:deploy .
docker image inspect mysql:8.4.10 >/dev/null
cp deploy/compose.yaml deploy/start.sh "$output/"
cp migrations/001_init.sql "$output/"
docker save audio-recording:deploy mysql:8.4.10 | gzip > "$output/images.tar.gz"
tar -C "$output" -czf "$output.tar.gz" compose.yaml start.sh 001_init.sql images.tar.gz
printf '部署包：%s.tar.gz\n' "$output"
