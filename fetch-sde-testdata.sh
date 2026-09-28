#!/usr/bin/env bash
# Downloads the EVE Online SDE and extracts only the files the Go tests need
# (mapSolarSystems.jsonl / mapStargates.jsonl) into backend/sde/.
#
# The SDE itself is gitignored and normally populated by the backend on startup.
# Run this before `go test ./...` from backend/.
set -euo pipefail

cd "$(dirname "$0")"

SDE_URL="${SDE_URL:-https://developers.eveonline.com/static-data/eve-online-static-data-latest-jsonl.zip}"
SDE_DIR="backend/sde"
SDE_ZIP="backend/sde.zip"
FORCE=0

for arg in "$@"; do
  case "$arg" in
    -f|--force) FORCE=1 ;;
    -h|--help)
      echo "Usage: $0 [--force]"
      echo "  --force  re-download and re-extract even if the data is already present"
      exit 0
      ;;
    *)
      echo "Usage: $0 [--force]"
      exit 1
      ;;
  esac
done

SYSTEMS_FILE="$SDE_DIR/mapSolarSystems.jsonl"
STARGATES_FILE="$SDE_DIR/mapStargates.jsonl"

if [ "$FORCE" -eq 0 ] && [ -s "$SYSTEMS_FILE" ] && [ -s "$STARGATES_FILE" ]; then
  echo "SDE test data already present in $SDE_DIR (use --force to refresh)."
  exit 0
fi

CURL_ARGS=(-fL --retry 3 --retry-delay 2 --progress-bar)
if [ -n "${SOCKS5_PROXY:-}" ]; then
  echo "Using SOCKS5 proxy $SOCKS5_PROXY"
  CURL_ARGS+=(--socks5-hostname "$SOCKS5_PROXY")
fi

if [ -s "$SDE_ZIP" ]; then
  echo "Reusing existing $SDE_ZIP"
else
  echo "Downloading SDE from $SDE_URL ..."
  curl "${CURL_ARGS[@]}" -o "$SDE_ZIP" "$SDE_URL"
fi

mkdir -p "$SDE_DIR"

if command -v unzip >/dev/null 2>&1; then
  # -j discards the archive paths so both files land directly in backend/sde/
  unzip -o -j "$SDE_ZIP" '*mapSolarSystems.jsonl' '*mapStargates.jsonl' -d "$SDE_DIR"
else
  echo "unzip not found, extracting with python3"
  python3 - "$SDE_ZIP" "$SDE_DIR" <<'PY'
import sys, zipfile

zip_path, out_dir = sys.argv[1], sys.argv[2]
wanted = ("mapSolarSystems.jsonl", "mapStargates.jsonl")
with zipfile.ZipFile(zip_path) as zf:
    names = {n for n in wanted if any(i.endswith(n) for i in zf.namelist())}
    missing = set(wanted) - names
    if missing:
        sys.exit("missing from SDE: " + ", ".join(sorted(missing)))
    for name in wanted:
        with zf.open(name) as src, open(f"{out_dir}/{name}", "wb") as dst:
            dst.write(src.read())
PY
fi

for f in "$SYSTEMS_FILE" "$STARGATES_FILE"; do
  if [ ! -s "$f" ]; then
    echo "ERROR: $f was not extracted." >&2
    exit 1
  fi
  if ! head -n 1 "$f" | grep -q '"_key"'; then
    echo "ERROR: $f does not look like SDE JSONL data." >&2
    exit 1
  fi
  echo "OK: $f ($(wc -l <"$f" | tr -d ' ') lines)"
done

echo "Done. Run: cd backend && go test ./..."
