#!/usr/bin/env bash
# Conformance suite (see docs/architecture.md, "Verifying there is no lock-in"): checks a sealed week with
# independent tools only.
# Usage: scripts/conformance.sh <week directory>          weekly/<year>/W<week>/ with WACZ volumes and SHA256SUMS
#        scripts/conformance.sh <package.wacz> [package.wacz.sha256]
# Requires: unzip, zipinfo, sha256sum, python3; warcio and wacz (pip install warcio wacz);
# pywb is used for a replay check of the whole week when installed.
set -euo pipefail

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }
ok() { echo "ok   $*"; }

if [[ -d "$1" ]]; then
  dir=$(realpath "$1")
  packages=("$dir"/*.wacz)
  [[ -f "${packages[0]}" ]] || fail "no WACZ volume in $dir"
  # 1. checksums of the volumes
  [[ -f "$dir/SHA256SUMS" ]] || fail "no SHA256SUMS in $dir"
  (cd "$dir" && sha256sum -c SHA256SUMS) >/dev/null || fail "sha256sum -c SHA256SUMS"
  [[ $(wc -l < "$dir/SHA256SUMS") == "${#packages[@]}" ]] || fail "SHA256SUMS does not list every volume"
  ok "SHA256SUMS matches ${#packages[@]} volume(s)"
else
  packages=("$(realpath "$1")")
  sumfile=${2:-}
  if [[ -n "$sumfile" ]]; then
    (cd "$(dirname "${packages[0]}")" && sha256sum -c "$(realpath "$sumfile")") >/dev/null || fail "sha256sum -c"
    ok "package sha256 matches"
  fi
fi

mkdir -p "$work/warc"
: > "$work/pages.jsonl"
for pkg in "${packages[@]}"; do
  name=$(basename "$pkg")
  out="$work/pkg/$name"

  # 2. every ZIP entry is stored without compression
  methods=$(zipinfo -l "$pkg" | awk 'NF>=8 && $1 ~ /^[-d]/ {print $7}' | sort -u)
  [[ -z "$methods" ]] && fail "$name: no entries listed by zipinfo"
  for m in $methods; do [[ "$m" == "stor" ]] || fail "$name: entry compressed with method $m"; done
  ok "$name: all entries stored (zipinfo)"

  # 3. datapackage.json hashes match the entries
  mkdir -p "$out"
  unzip -q "$pkg" -d "$out"
  python3 - "$out" <<'PY' || fail "$name: datapackage.json hashes"
import hashlib, json, sys, os
root = sys.argv[1]
dp = json.load(open(os.path.join(root, "datapackage.json")))
for r in dp["resources"]:
    data = open(os.path.join(root, r["path"]), "rb").read()
    h = "sha256:" + hashlib.sha256(data).hexdigest()
    assert h == r["hash"], (r["path"], h, r["hash"])
    assert len(data) == r["bytes"], r["path"]
dd = json.load(open(os.path.join(root, "datapackage-digest.json")))
raw = open(os.path.join(root, "datapackage.json"), "rb").read()
assert dd["hash"] == "sha256:" + hashlib.sha256(raw).hexdigest()
PY
  ok "$name: datapackage.json hashes (sha256)"

  # 4. CDXJ sorted bytewise
  if [[ -f "$out/indexes/index.cdxj" ]]; then
    LC_ALL=C sort -c "$out/indexes/index.cdxj" || fail "$name: index.cdxj not sorted"
    ok "$name: index.cdxj sorted (LC_ALL=C sort -c)"
  fi

  # 5. WARC structure and digests with warcio; WARC names are unique in the week
  for warc in "$out"/archive/*.warc.gz; do
    out_check=$(warcio check -v "$warc") || fail "warcio check $warc"
    grep -q "digest failed" <<<"$out_check" && fail "digest failure in $warc"
    warcio index "$warc" >/dev/null || fail "warcio index $warc"
    [[ -e "$work/warc/$(basename "$warc")" ]] && fail "$(basename "$warc") is in two volumes"
    cp "$warc" "$work/warc/"
  done
  ok "$name: warcio check/index on $(ls "$out"/archive | wc -l) WARC file(s)"

  # 6. WACZ validation with py-wacz
  wacz validate -f "$pkg" >/dev/null || fail "$name: wacz validate"
  ok "$name: wacz validate"
  tail -n +2 "$out/pages/pages.jsonl" >> "$work/pages.jsonl"
done

# 7. replay of the whole week with pywb (optional): a revisit may point into another volume
if command -v wb-manager >/dev/null && command -v wayback >/dev/null; then
  (
    cd "$work"
    wb-manager init conf >/dev/null
    wb-manager add conf "$work"/warc/*.warc.gz >/dev/null
    url=$(python3 -c 'import json,sys
for l in open(sys.argv[1]):
    p=json.loads(l)
    if p.get("url","").startswith("http") and p.get("kind","page")=="page" and p.get("status",200)==200: print(p["url"]); break' "$work/pages.jsonl")
    port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')
    wayback -p "$port" -b 127.0.0.1 >/dev/null 2>&1 &
    pid=$!
    trap 'kill $pid 2>/dev/null' EXIT
    for _ in $(seq 50); do curl -sf "http://127.0.0.1:$port/" >/dev/null && break; sleep 0.2; done
    code=$(curl -s -o "$work/replay.html" -w '%{http_code}' "http://127.0.0.1:$port/conf/mp_/$url")
    [[ "$code" == "200" ]] || { echo "pywb returned $code for $url" >&2; exit 1; }
  ) || fail "pywb replay"
  ok "pywb replay"
elif [[ "${HOARFROST_CONFORMANCE:-}" == "required" ]]; then
  fail "pywb is required"
else
  echo "skip pywb replay (pywb not installed)"
fi
echo "conformance: all checks passed"
