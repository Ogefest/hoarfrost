#!/usr/bin/env bash
# Exit test (see docs/architecture.md, "Verifying there is no lock-in"): read the archive as if hoarfrost no longer
# existed, using only standard tools: rclone (or a local path), unzip, dd,
# gunzip, grep, sha256sum, warcio and pywb. No hoarfrost code is used.
#
# A week is stored as WACZ volumes in weekly/<year>/W<week>/ (weeks sealed
# before volumes: one weekly/<year>/W<week>.wacz). The whole week is
# loaded, as a revisit in one volume may point to a record in another.
#
# Usage: scripts/exit-test.sh <storage> [export.html]
#   <storage>   local archive root, or an rclone remote such as s3remote:bucket/prefix
#   export.html optional export of the same article made by hoarfrost, to compare text
set -euo pipefail

src=$1
compare=${2:-}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
step() { echo "== $*"; }

step "1. pick a random week and fetch its packages"
mkdir -p "$work/week"
if [[ -d "$src" ]]; then
  # A week's directory is complete once SHA256SUMS is written.
  weeks=$(cd "$src" && { ls weekly/*/W*/SHA256SUMS 2>/dev/null | sed 's#SHA256SUMS$##'; ls weekly/*/W*.wacz 2>/dev/null; } || true)
else
  weeks=$(rclone lsf -R --files-only --include 'SHA256SUMS' "$src/weekly" | grep -E '^[0-9]{4}/W[0-9]{2}/SHA256SUMS$' | sed 's#SHA256SUMS$##; s#^#weekly/#' || true)
  weeks+=$'\n'$(rclone lsf -R --files-only --include '*.wacz' "$src/weekly" | grep -E '^[0-9]{4}/W[0-9]{2}\.wacz$' | sed 's#^#weekly/#' || true)
fi
pick=$(grep -v '^$' <<<"$weeks" | shuf -n1)
[[ -n "$pick" ]] || { echo "no sealed week in $src" >&2; exit 1; }
if [[ -d "$src" ]]; then
  if [[ "$pick" == */ ]]; then
    cp "$src/$pick"* "$work/week/"
  else
    cp "$src/$pick" "$work/week/"
    if [[ -f "$src/$pick.sha256" ]]; then cp "$src/$pick.sha256" "$work/week/"; fi
  fi
else
  if [[ "$pick" == */ ]]; then rclone copy "$src/$pick" "$work/week"; else rclone copy "$src/$(dirname "$pick")" "$work/week" --include "$(basename "$pick")*"; fi
fi
echo "week: $pick"
ls -l "$work/week"
(
  cd "$work/week"
  if [[ -f SHA256SUMS ]]; then sha256sum -c SHA256SUMS; else for s in *.sha256; do [[ -f "$s" ]] && sha256sum -c "$s"; done; fi
)
packages=("$work"/week/*.wacz)

step "2. list contents and articles"
: > "$work/pages.jsonl"
for p in "${packages[@]}"; do
  unzip -l "$p"
  unzip -p "$p" pages/pages.jsonl >> "$work/pages.jsonl"
done
grep -o '"url":"[^"]*","[^}]*"title":"[^"]*"' "$work/pages.jsonl" | head -20 || true
if [[ -n "$compare" ]]; then
  wanted=$(grep -o '<title>[^<]*</title>' "$compare" | head -1 | sed -E 's#</?title>##g')
  article=$(grep -F "\"title\":\"$wanted\"" "$work/pages.jsonl" | head -1)
else
  article=$(grep -v '"format"' "$work/pages.jsonl" | grep '"kind":"page"' | grep '"status":200' | head -1)
fi
[[ -n "$article" ]] || { echo "article not found in pages.jsonl" >&2; exit 1; }
url=$(sed -E 's/.*"url":"([^"]*)".*/\1/' <<<"$article")
title=$(sed -E 's/.*"title":"([^"]*)".*/\1/' <<<"$article")
echo "article: $title <$url>"

step "3. find the record in index.cdxj and cut it out with dd"
line="" pkg=""
for p in "${packages[@]}"; do
  line=$(unzip -p "$p" indexes/index.cdxj | grep -F "\"url\":\"$url\"" | grep -E '"(mime)":"text/html"' | head -1 || true)
  if [[ -n "$line" ]]; then pkg=$p; break; fi
done
[[ -n "$line" ]] || { echo "article not found in any index.cdxj" >&2; exit 1; }
offset=$(sed -E 's/.*"offset":"([0-9]+)".*/\1/' <<<"$line")
length=$(sed -E 's/.*"length":"([0-9]+)".*/\1/' <<<"$line")
file=$(sed -E 's/.*"filename":"([^"]+)".*/\1/' <<<"$line")
unzip -p "$pkg" "archive/$file" > "$work/file.warc.gz"
dd if="$work/file.warc.gz" of="$work/record.warc.gz" bs=1 skip="$offset" count="$length" status=none
gunzip -c "$work/record.warc.gz" > "$work/record.warc"
grep -q "WARC-Target-URI: $url" "$work/record.warc"
grep -qF "$title" <(gunzip -c "$work/record.warc.gz" | warcio extract --payload /dev/stdin 0 2>/dev/null || cat "$work/record.warc")
warcio check "$work/record.warc.gz"
echo "record found in $(basename "$pkg") at $file:$offset ($length bytes)"

step "4. replay the week with pywb"
(
  cd "$work"
  for p in "${packages[@]}"; do unzip -q -o "$p" 'archive/*' -d pkg; done
  wb-manager init exit >/dev/null
  wb-manager add exit pkg/archive/*.warc.gz >/dev/null
  port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')
  wayback -p "$port" -b 127.0.0.1 >/dev/null 2>&1 &
  pid=$!
  trap 'kill $pid 2>/dev/null' EXIT
  for _ in $(seq 50); do curl -sf "http://127.0.0.1:$port/" >/dev/null && break; sleep 0.2; done
  curl -sf "http://127.0.0.1:$port/exit/mp_/$url" > replay.html
  grep -qF "$title" replay.html
)
echo "pywb replays the article from the ${#packages[@]} package(s) of the week"

step "5. compare with the application's export"
if [[ -n "$compare" ]]; then
  text=$(sed -E 's/.*"text":"([^"]*)".*/\1/' <<<"$article" | sed 's/\\n/ /g')
  words=$(LC_ALL=C.UTF-8 grep -oE '[[:alpha:]]{7,}' <<<"$text" | head -8)
  [[ -n "$words" ]] || { echo "no text in pages.jsonl" >&2; exit 1; }
  grep -qF "$title" "$compare" || { echo "title missing from export" >&2; exit 1; }
  for w in $words; do
    grep -qF "$w" "$compare" || { echo "\"$w\" missing from export" >&2; exit 1; }
    grep -qF "$w" "$work/replay.html" || { echo "\"$w\" missing from replay" >&2; exit 1; }
  done
  snippet=$(tr '\n' ' ' <<<"$words")
  echo "export and pywb replay contain the same title and text (\"$snippet\")"
else
  echo "no export given, skipped"
fi
echo "exit test passed"
