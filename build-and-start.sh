#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

echo "$(date '+%Y-%m-%d %H:%M:%S %z')  $0"

STAND="${1:-test}"
case "$STAND" in
  prod|test) ;;
  *)
    echo "Usage: $0 [prod|test]"
    exit 1
    ;;
esac

echo "Bringing down existing stack (with volumes)..."
docker compose down -v

echo "Running build.sh $STAND..."
TMPFILE=$(mktemp)
trap "rm -f '$TMPFILE'" EXIT
bash build.sh "$STAND" 2>&1 | tee "$TMPFILE"

# Extract IMAGE_TAG from build.sh output
IMAGE_TAG=$(grep "IMAGE_TAG (embedded in backend):" "$TMPFILE" | sed 's/.*: //')
export IMAGE_TAG

echo "Using IMAGE_TAG: ${IMAGE_TAG}"

echo "Starting stack (attached, ^C to stop)..."
docker compose up --force-recreate
