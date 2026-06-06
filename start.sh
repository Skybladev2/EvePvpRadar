#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

echo "$(date '+%Y-%m-%d %H:%M:%S %z')  $0"

# Load .env if present so variables like IMAGE_TAG / image repos can be defined there.
if [ -f .env ]; then
  set -a
  . ./.env
  set +a
fi

# IMAGE_TAG must be set explicitly (make sure .env didn't set it to empty)
if [ -z "${IMAGE_TAG:-}" ]; then
  echo "IMAGE_TAG must be set (run build.sh first, or pass it explicitly)" >&2
  exit 1
fi

echo "Using IMAGE_TAG: ${IMAGE_TAG}"

# Parse optional -d flag for detached mode
DETACHED=""
if [ "${1:-}" = "-d" ]; then
    DETACHED="-d"
    shift
fi

echo "Starting stack without building or pushing images..."
docker compose up ${DETACHED} --no-build --force-recreate
