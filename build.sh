#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

echo "$(date '+%Y-%m-%d %H:%M:%S %z')  $0"

STAND="${1:-}"
case "$STAND" in
  prod|test) ;;
  *)
    echo "Usage: $0 <prod|test>"
    echo "  prod — use .env.prod (SSO from PROD section in .env)"
    echo "  test — use .env.test (SSO from TEST section in .env)"
    exit 1
    ;;
esac

./generate-env-stands.sh

ENV_FILE=".env.${STAND}"
if [ ! -f "$ENV_FILE" ]; then
  echo "ERROR: $ENV_FILE not found. Create .env and re-run, or copy an existing stand env file."
  exit 1
fi

echo "Using stand env: $ENV_FILE"
set -a
# shellcheck disable=SC1090
. /dev/stdin <<<"$(tr -d '\r' < "./$ENV_FILE")"
set +a

export ENV_FILE

# Require image repos to be explicitly provided via environment
: "${FRONTEND_IMAGE:?FRONTEND_IMAGE environment variable must be set (e.g. skyblade/evepvpradar-frontend)}"
: "${BACKEND_IMAGE:?BACKEND_IMAGE environment variable must be set (e.g. skyblade/evepvpradar-backend)}"

export FRONTEND_IMAGE
export BACKEND_IMAGE

# Determine IMAGE_TAG:
#   - If working copy is clean -> use commit SHA
#   - If dirty and HEAD has a tag -> use <tag>-post-commit-<datetime>
#   - If dirty with no tag at HEAD -> use <SHA>-post-commit-<datetime>
SHA="$(git rev-parse HEAD)"
if ! git diff --quiet || ! git diff --cached --quiet; then
  DT="$(date +%Y%m%d%H%M%S)"
  TAG="$(git tag --points-at HEAD | sort -V | tail -1)"
  if [ -n "$TAG" ]; then
    IMAGE_TAG="${TAG}-post-commit-${DT}"
  else
    IMAGE_TAG="${SHA}-post-commit-${DT}"
  fi
  echo "Dirty working copy, tagging with: $IMAGE_TAG"
else
  IMAGE_TAG="$SHA"
  echo "Working copy is clean, using commit SHA as tag: $IMAGE_TAG"
fi
export IMAGE_TAG
echo "IMAGE_TAG (embedded in backend): $IMAGE_TAG"

echo "FRONTEND_IMAGE: $FRONTEND_IMAGE"
echo "BACKEND_IMAGE:  $BACKEND_IMAGE"

echo "Checking third-party monitoring/security images for safe updates..."
set +e
ENV_FILE=".env" bash ./check-third-party-images.sh
CHECK_EXIT_CODE=$?
set -e

case "$CHECK_EXIT_CODE" in
  0)
    ;;
  100)
    echo "Safe third-party digest updates applied. Regenerating stand env from updated .env..."
    rm -f "$ENV_FILE"
    ./generate-env-stands.sh
    set -a
    # shellcheck disable=SC1090
    . /dev/stdin <<<"$(tr -d '\r' < "./$ENV_FILE")"
    set +a
    ;;
  101)
    echo "Unsafe third-party update detected. Aborting build."
    exit 1
    ;;
  *)
    echo "Third-party image check failed with exit code $CHECK_EXIT_CODE."
    exit "$CHECK_EXIT_CODE"
    ;;
esac

# Check if docker compose supports --no-cache (v2.24+)
# Older versions and the legacy docker-compose don't recognize this flag.
if docker compose build --help 2>&1 | grep -q no-cache; then
  NO_CACHE="--no-cache"
else
  echo "docker compose does not support --no-cache, building without it"
  NO_CACHE=""
fi

echo "Building images${NO_CACHE:+" with no cache"}..."
# shellcheck disable=SC2086
docker compose build $NO_CACHE --provenance=false --platform linux/amd64 frontend backend

echo "Pushing images to container registry..."
docker compose push frontend backend
