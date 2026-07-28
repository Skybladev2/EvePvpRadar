#!/usr/bin/env bash
set -euo pipefail

echo "$(date '+%Y-%m-%d %H:%M:%S %z')  $0"

ENV_FILE="${ENV_FILE:-.env}"
SCANNERS_ENV_FILE="${SCANNERS_ENV_FILE:-${CLAMAV_ENV_FILE:-.env.scanners}}"

if [ ! -f "$ENV_FILE" ]; then
  echo "ERROR: $ENV_FILE was not found."
  exit 1
fi

if [ ! -f "$SCANNERS_ENV_FILE" ]; then
  echo "ERROR: $SCANNERS_ENV_FILE was not found (Trivy/ClamAV scanner settings for this maintenance script)."
  exit 1
fi

set -a
# Load env files with CR stripped so Windows CRLF does not break `source`.
# ShellCheck correctly warns about SC1091 here; the files are shell-safe.
# NOTE: . <(cmd) does NOT work on macOS bash 3.2, so use heredoc + /dev/stdin instead.
# shellcheck disable=SC1091
. /dev/stdin <<<"$(tr -d '\r' < "./$ENV_FILE")"
# shellcheck disable=SC1091
. /dev/stdin <<<"$(tr -d '\r' < "./$SCANNERS_ENV_FILE")"
set +a

required_vars=(
  NGINX_EXPORTER_IMAGE_REPO
  NGINX_EXPORTER_IMAGE_TAG
  NGINX_EXPORTER_IMAGE_DIGEST
  PROMETHEUS_IMAGE_REPO
  PROMETHEUS_IMAGE_TAG
  PROMETHEUS_IMAGE_DIGEST
  GRAFANA_IMAGE_REPO
  GRAFANA_IMAGE_TAG
  GRAFANA_IMAGE_DIGEST
  TRIVY_SCAN_IMAGE_REPO
  TRIVY_SCAN_IMAGE_TAG
  TRIVY_SCAN_IMAGE_DIGEST
  CLAMAV_SCAN_IMAGE_REPO
  CLAMAV_SCAN_IMAGE_TAG
  CLAMAV_SCAN_IMAGE_DIGEST
)

missing_vars=()
for v in "${required_vars[@]}"; do
  # `${!v:-}` is safe under `set -u` and treats empty strings as missing.
  if [ -z "${!v:-}" ]; then
    missing_vars+=("$v")
  fi
done

if [ "${#missing_vars[@]}" -gt 0 ]; then
  echo "ERROR: missing required environment variables (from $ENV_FILE):"
  for v in "${missing_vars[@]}"; do
    echo "  - $v"
  done
  exit 1
fi

# Strip CR from values (Windows CRLF in .env breaks Docker Hub URLs and docker pull).
for v in "${required_vars[@]}"; do
  val="${!v}"
  val="${val//$'\r'/}"
  printf -v "$v" '%s' "$val"
done

CLAMAV_SCANNER_IMAGE="${CLAMAV_SCAN_IMAGE_REPO}@${CLAMAV_SCAN_IMAGE_DIGEST}"
THIRD_PARTY_IMAGE_MIN_AGE_DAYS="${THIRD_PARTY_IMAGE_MIN_AGE_DAYS:-30}"
ENABLE_MALWARE_SCAN="${ENABLE_MALWARE_SCAN:-1}"
CLAMAV_REFRESH_DB="${CLAMAV_REFRESH_DB:-1}"
CLAMAV_FRESHCLAM_MAX_AGE_SECONDS="${CLAMAV_FRESHCLAM_MAX_AGE_SECONDS:-86400}"
CLAMAV_STATE_DIR="${CLAMAV_STATE_DIR:-./clamav-state}"
CLAMAV_LAST_FRESHCLAM_RUN_FILE="${CLAMAV_LAST_FRESHCLAM_RUN_FILE:-${CLAMAV_STATE_DIR}/.freshclam-last-run}"

THIRD_PARTY_IMAGE_MIN_AGE_DAYS="${THIRD_PARTY_IMAGE_MIN_AGE_DAYS//$'\r'/}"
ENABLE_MALWARE_SCAN="${ENABLE_MALWARE_SCAN//$'\r'/}"
CLAMAV_REFRESH_DB="${CLAMAV_REFRESH_DB//$'\r'/}"
CLAMAV_FRESHCLAM_MAX_AGE_SECONDS="${CLAMAV_FRESHCLAM_MAX_AGE_SECONDS//$'\r'/}"
CLAMAV_STATE_DIR="${CLAMAV_STATE_DIR//$'\r'/}"
CLAMAV_LAST_FRESHCLAM_RUN_FILE="${CLAMAV_LAST_FRESHCLAM_RUN_FILE//$'\r'/}"

should_run_freshclam() {
  if [ "$CLAMAV_REFRESH_DB" != "1" ]; then
    return 1
  fi

  local max_age_seconds="${CLAMAV_FRESHCLAM_MAX_AGE_SECONDS:-86400}"
  local now_epoch last_epoch age_seconds

  now_epoch="$(date +%s)"
  if [ ! -f "$CLAMAV_LAST_FRESHCLAM_RUN_FILE" ]; then
    return 0
  fi

  # State is epoch seconds stored as a single line.
  if ! read -r last_epoch < "$CLAMAV_LAST_FRESHCLAM_RUN_FILE"; then
    return 0
  fi

  if [ -z "$last_epoch" ]; then
    return 0
  fi

  age_seconds=$((now_epoch - last_epoch))
  echo "  - freshclam state: file=$CLAMAV_LAST_FRESHCLAM_RUN_FILE last_run=$(date -r "$last_epoch" '+%Y-%m-%d %H:%M:%S' 2>/dev/null || echo "$last_epoch") age=${age_seconds}s max=${max_age_seconds}s"
  [ "$age_seconds" -ge "$max_age_seconds" ]
}

mark_freshclam_attempt() {
  mkdir -p "$CLAMAV_STATE_DIR"
  date +%s > "$CLAMAV_LAST_FRESHCLAM_RUN_FILE"
}

update_env_var() {
  local key="$1"
  local value="$2"
  local target="${3:-$ENV_FILE}"
  local tmp_file
  tmp_file="$(mktemp)"

  if grep -q "^${key}=" "$target"; then
    awk -v k="$key" -v v="$value" '
      BEGIN { updated = 0 }
      $0 ~ ("^" k "=") { print k "=" v; updated = 1; next }
      { print }
      END { if (!updated) print k "=" v }
    ' "$target" > "$tmp_file"
    mv "$tmp_file" "$target"
  else
    printf "\n%s=%s\n" "$key" "$value" >> "$target"
  fi
}

extract_digest() {
  local image_ref="$1"
  local digest_line digest
  digest_line="$(docker image inspect --format '{{index .RepoDigests 0}}' "$image_ref" 2>/dev/null || true)"
  digest="${digest_line##*@}"
  if [ -z "$digest" ] || [ "$digest" = "$digest_line" ]; then
    echo "ERROR: could not resolve digest for $image_ref"
    return 1
  fi
  printf "%s" "$digest"
}

dockerhub_tag_update_gate() {
  local repo="$1"
  local tag_name="$2"
  local current_digest="$3"
  local min_age_days="$4"

  python3 - "$repo" "$tag_name" "$current_digest" "$min_age_days" <<'PY'
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime, timedelta, timezone

_socks5_proxy = os.environ.get("BUILD_SOCKS5_PROXY", "").strip()
if _socks5_proxy:
    try:
        import socks as _socks
        import socket as _socket

        _proxy_url = _socks5_proxy
        for _prefix in ("socks5h://", "socks5://", "socks://"):
            if _proxy_url.startswith(_prefix):
                _proxy_url = _proxy_url[len(_prefix):]
                break
        _host, _port_str = _proxy_url.rsplit(":", 1)
        _port = int(_port_str)
        _socks.set_default_proxy(_socks.SOCKS5, _host, _port)
        _socket.socket = _socks.socksocket
    except ImportError:
        print("WARNING: BUILD_SOCKS5_PROXY is set but PySocks is not installed (pip install PySocks)", file=sys.stderr)
    except Exception as _e:
        print(f"WARNING: failed to configure SOCKS5 proxy {_socks5_proxy}: {_e}", file=sys.stderr)

def _parse_ts(s):
    s = s.replace("Z", "+00:00")
    dot = s.find(".")
    if dot != -1:
        i = dot + 1
        while i < len(s) and s[i].isdigit():
            i += 1
        frac = s[dot + 1 : i]
        if len(frac) != 6:
            s = s[: dot + 1] + frac[:6].ljust(6, "0") + s[i:]
    try:
        return datetime.fromisoformat(s)
    except ValueError:
        pass
    for sep in ("+", "-"):
        idx = s.rfind(sep)
        if idx > 19:
            s = s[:idx]
            break
    return datetime.fromisoformat(s).replace(tzinfo=timezone.utc)

repo = sys.argv[1]
tag_name = sys.argv[2]
current_digest = sys.argv[3]
min_age_days = int(sys.argv[4])
cutoff = datetime.now(timezone.utc) - timedelta(days=min_age_days)

list_url = f"https://hub.docker.com/v2/repositories/{repo}/tags/?page_size=100"
try:
    with urllib.request.urlopen(list_url, timeout=30) as resp:
        all_tags = json.load(resp).get("results", [])
except urllib.error.HTTPError as e:
    print(f"error\tHTTP {e.code} fetching tags for {repo}", file=sys.stderr)
    sys.exit(1)
except Exception as e:
    print(f"error\tfailed to list tags for {repo}: {e}", file=sys.stderr)
    sys.exit(1)

if not all_tags:
    print(f"error\tno tags found for {repo}", file=sys.stderr)
    sys.exit(1)

digest_pushes = {}
for t in all_tags:
    d = (t.get("digest") or "").strip()
    pushed = t.get("tag_last_pushed") or t.get("last_updated") or ""
    if d and pushed:
        if d not in digest_pushes or pushed < digest_pushes[d]:
            digest_pushes[d] = pushed

sorted_items = sorted(digest_pushes.items(), key=lambda x: x[1])

current_idx = None
for i, (d, _) in enumerate(sorted_items):
    if d == current_digest:
        current_idx = i
        break

if current_idx is not None and current_idx < len(sorted_items) - 1:
    next_digest, next_pushed = sorted_items[current_idx + 1]
    next_ts = _parse_ts(next_pushed)
    if next_ts <= cutoff:
        print(f"ok\t{next_digest}")
    else:
        print(f"too_new\t{next_digest}\t{next_pushed}")
    sys.exit(0)

# current_digest not found or is the newest — check if the tag itself changed
enc_tag = urllib.parse.quote(tag_name, safe="")
url = f"https://hub.docker.com/v2/repositories/{repo}/tags/{enc_tag}/"
try:
    with urllib.request.urlopen(url, timeout=30) as resp:
        tag_data = json.load(resp)
except urllib.error.HTTPError as e:
    print(f"error\tHTTP {e.code} fetching {url}", file=sys.stderr)
    sys.exit(1)

remote_digest = (tag_data.get("digest") or "").strip()
if not remote_digest:
    print("error\tno digest in Docker Hub response", file=sys.stderr)
    sys.exit(1)
if remote_digest == current_digest:
    print("unchanged")
    sys.exit(0)

oldest_push = None
for t in all_tags:
    if (t.get("digest") or "").strip() == remote_digest:
        pushed = t.get("tag_last_pushed") or t.get("last_updated") or ""
        if pushed:
            try:
                ts = _parse_ts(pushed)
                if oldest_push is None or ts < oldest_push:
                    oldest_push = ts
            except ValueError:
                pass

if oldest_push is None:
    pushed_val = tag_data.get("tag_last_pushed") or tag_data.get("last_updated") or ""
    if pushed_val:
        try:
            oldest_push = _parse_ts(pushed_val)
        except ValueError:
            oldest_push = None

if oldest_push is None or oldest_push > cutoff:
    reason = "unknown age" if oldest_push is None else oldest_push.isoformat()
    print(f"too_new\t{remote_digest}\t{reason}")
    sys.exit(0)

print(f"ok\t{remote_digest}")
sys.exit(0)
PY
}

CLAMAV_FRESHCLAM_DONE=0

run_freshclam_if_needed() {
  if [ "$ENABLE_MALWARE_SCAN" != "1" ]; then
    return
  fi
  if [ "$CLAMAV_FRESHCLAM_DONE" = "1" ]; then
    return
  fi
  if ! should_run_freshclam; then
    return
  fi
  echo "Updating ClamAV virus definitions..."
  if docker run --rm "$CLAMAV_SCANNER_IMAGE" freshclam --stdout; then
    mark_freshclam_attempt
    CLAMAV_FRESHCLAM_DONE=1
  else
    echo "WARNING: ClamAV database update failed, proceeding with current definitions"
  fi
}

scan_candidate_for_malware() {
  local image_ref="$1"
  local work_dir="" export_tar rootfs_dir container_id tmp_output=""
  # Always clean up temporary extracted image contents and any ClamAV output.
  # Requirement: if ClamAV finds malware, do not keep the report directory for later analysis.
  trap 'if [ -n "${work_dir:-}" ] && [ -d "${work_dir:-}" ]; then rm -rf "${work_dir:-}"; fi' RETURN

  if [ "$ENABLE_MALWARE_SCAN" != "1" ]; then
    echo "  - malware scan skipped (ENABLE_MALWARE_SCAN=${ENABLE_MALWARE_SCAN})"
    return 0
  fi

  work_dir="$(mktemp -d)"
  export_tar="${work_dir}/image.tar"
  rootfs_dir="${work_dir}/rootfs"
  tmp_output="${work_dir}/clamav.out"
  mkdir -p "$rootfs_dir"

  container_id="$(docker create "$image_ref" sh -c 'true')"
  docker export "$container_id" -o "$export_tar" >/dev/null
  docker rm "$container_id" >/dev/null
  tar -xf "$export_tar" -C "$rootfs_dir"

  # ClamAV exit codes:
  #  0: OK (no malware)
  #  1: malware found
  #  2: error
  local clamscan_rc=0
  run_freshclam_if_needed
  echo "  - scanning ${image_ref}"

  clamscan_rc=0
  docker run --rm \
    -v "${rootfs_dir}:/scan:ro" \
    "$CLAMAV_SCANNER_IMAGE" \
    sh -c 'clamscan -r --infected --no-summary /scan' >"$tmp_output" 2>&1 \
    || clamscan_rc=$?

  if [ "$clamscan_rc" -eq 0 ]; then
    return 0
  fi

  if [ "$clamscan_rc" -eq 1 ]; then
    local report_dir="./clamav-reports"
    local report_ts
    local safe_image_ref
    local report_file

    report_ts="$(date +%Y%m%d_%H%M%S)"
    safe_image_ref="${image_ref//[^a-zA-Z0-9._-]/_}"
    report_file="${report_dir}/clamav-${safe_image_ref}-${report_ts}.txt"

    mkdir -p "$report_dir"
    # Persist only the report text (ClamAV output) when malware is found.
    cp "$tmp_output" "$report_file"

    echo "  - clamav findings for ${image_ref}:"
    sed 's/^/    /' "$tmp_output"
    echo "  - clamav report saved to ${report_file}"
  else
    echo "  - clamav error (exit code: ${clamscan_rc}) for ${image_ref}:"
    sed 's/^/    /' "$tmp_output"
  fi
  return 1
}

check_one() {
  local name="$1"
  local repo="$2"
  local tag="$3"
  local current_digest="$4"

  echo "Starting image check: ${name}"

  local updates_applied=0
  local first_applied_digest=""

  while true; do
    local gate_line gate_status
    gate_line="$(dockerhub_tag_update_gate "$repo" "$tag" "$current_digest" "$THIRD_PARTY_IMAGE_MIN_AGE_DAYS")" || {
      echo "WARNING: Docker Hub metadata lookup failed for ${name} (${repo}:${tag}); skipping this image for now"
      local remaining=()
      for item in "${safe_updates[@]}"; do
        [[ "$item" != "${name}|"* ]] && remaining+=("$item")
      done
      safe_updates=("${remaining[@]}")
      skipped_updates+=("${name}|docker hub metadata lookup failed")
      return 0
    }
    gate_status="${gate_line%%$'\t'*}"
    case "$gate_status" in
      unchanged)
        if [ "$updates_applied" -eq 0 ]; then
          echo "Checking ${name}: ${repo}:${tag}"
          echo "  - digest unchanged on Docker Hub"
        fi
        break
        ;;
      too_new)
        local remote_digest pushed_info
        IFS=$'\t' read -r gate_status remote_digest pushed_info <<<"$gate_line"
        if [ "$updates_applied" -eq 0 ]; then
          echo "Checking ${name}: ${repo}:${tag}"
        fi
        echo "  - next digest ${remote_digest} (pushed ${pushed_info}) is not yet ${THIRD_PARTY_IMAGE_MIN_AGE_DAYS} days old; updates caught up"
        break
        ;;
      ok)
        local candidate_digest
        IFS=$'\t' read -r gate_status candidate_digest <<<"$gate_line"
        local candidate_ref="${repo}@${candidate_digest}"

        if [ -n "$first_applied_digest" ] && [ "$candidate_digest" = "$first_applied_digest" ]; then
          echo "  - all available digests processed; caught up to latest known image"
          break
        fi

        if [ "$updates_applied" -eq 0 ]; then
          echo "Checking ${name}: ${repo}:${tag}"
        fi
        echo "  - candidate digest: ${candidate_digest}"

        docker pull "$candidate_ref" >/dev/null

        if ! scan_candidate_for_malware "$candidate_ref"; then
          echo "  - unsafe: malware scan failed"
          unsafe_updates+=("${name}|${candidate_digest}|clamav malware scan failed")
          return 0
        fi

        if [ "$ENABLE_MALWARE_SCAN" = "1" ]; then
          echo "  - safe: malware scan passed"
        else
          echo "  - safe: digest accepted (malware scan disabled)"
        fi

        safe_updates+=("${name}|${candidate_digest}")
        [ -z "$first_applied_digest" ] && first_applied_digest="$candidate_digest"
        current_digest="$candidate_digest"
        updates_applied="$((updates_applied + 1))"
        ;;
      *)
        echo "WARNING: unexpected gate status from Docker Hub helper for ${name}: ${gate_line}; skipping this image for now"
        local remaining=()
        for item in "${safe_updates[@]}"; do
          [[ "$item" != "${name}|"* ]] && remaining+=("$item")
        done
        safe_updates=("${remaining[@]}")
        skipped_updates+=("${name}|unexpected docker hub gate status")
        return 0
        ;;
    esac
  done
}

safe_updates=()
unsafe_updates=()
skipped_updates=()

check_one "nginx-exporter" "$NGINX_EXPORTER_IMAGE_REPO" "$NGINX_EXPORTER_IMAGE_TAG" "$NGINX_EXPORTER_IMAGE_DIGEST"
check_one "prometheus" "$PROMETHEUS_IMAGE_REPO" "$PROMETHEUS_IMAGE_TAG" "$PROMETHEUS_IMAGE_DIGEST"
check_one "grafana" "$GRAFANA_IMAGE_REPO" "$GRAFANA_IMAGE_TAG" "$GRAFANA_IMAGE_DIGEST"
check_one "clamav-scan" "$CLAMAV_SCAN_IMAGE_REPO" "$CLAMAV_SCAN_IMAGE_TAG" "$CLAMAV_SCAN_IMAGE_DIGEST"

if [ "${#unsafe_updates[@]}" -gt 0 ]; then
  echo
  echo "Unsafe image updates detected. env files were NOT changed:"
  for item in "${unsafe_updates[@]}"; do
    IFS='|' read -r name digest reason <<< "$item"
    echo "  - ${name}: ${digest} (${reason})"
  done
  echo "Stopping startup because at least one new image is not considered safe."
  exit 101
fi

if [ "${#skipped_updates[@]}" -gt 0 ]; then
  echo
  echo "Some image checks were skipped due to temporary metadata errors:"
  for item in "${skipped_updates[@]}"; do
    IFS='|' read -r name reason <<< "$item"
    echo "  - ${name}: ${reason}"
  done
fi

if [ "${#safe_updates[@]}" -gt 0 ]; then
  for item in "${safe_updates[@]}"; do
    IFS='|' read -r name digest <<< "$item"
    case "$name" in
      nginx-exporter) update_env_var "NGINX_EXPORTER_IMAGE_DIGEST" "$digest" "$ENV_FILE" ;;
      prometheus) update_env_var "PROMETHEUS_IMAGE_DIGEST" "$digest" "$ENV_FILE" ;;
      grafana) update_env_var "GRAFANA_IMAGE_DIGEST" "$digest" "$ENV_FILE" ;;
      clamav-scan) update_env_var "CLAMAV_SCAN_IMAGE_DIGEST" "$digest" "$SCANNERS_ENV_FILE" ;;
    esac
  done

  echo
  echo "Safe image updates were applied ($ENV_FILE and/or $SCANNERS_ENV_FILE):"
  for item in "${safe_updates[@]}"; do
    IFS='|' read -r name digest <<< "$item"
    echo "  - ${name}: ${digest}"
  done
  echo "Stopping startup. Re-run build-and-start.sh to continue with new digests."
  exit 100
fi

echo "No new third-party image digests were found."
exit 0
