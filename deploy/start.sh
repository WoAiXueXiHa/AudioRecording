#!/usr/bin/env bash
set -euo pipefail
cd -- "$(dirname -- "$0")"
if [ ! -f .env ]; then
  umask 077
  read -r -s -p '输入 DeepSeek API Key（输入不显示）: ' audio_key
  echo
  [[ "$audio_key" =~ ^[A-Za-z0-9_-]+$ ]] || { echo '密钥为空或格式不支持'; exit 1; }
  {
    printf 'MYSQL_PASSWORD=%s\n' "$(openssl rand -hex 24)"
    printf 'MYSQL_ROOT_PASSWORD=%s\n' "$(openssl rand -hex 24)"
    printf 'DEEPSEEK_API_KEY=%s\n' "$audio_key"
    printf 'WORKER_CONCURRENCY=3\n'
  } > .env
  unset audio_key
fi
chmod 600 .env
docker load -i images.tar.gz
docker compose -p audio-recording up -d --wait --wait-timeout 180
docker compose -p audio-recording ps
curl --noproxy '*' --fail http://127.0.0.1:8080/health
