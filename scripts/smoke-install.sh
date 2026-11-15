#!/usr/bin/env bash
# Installs hoarfrost the way docs/install.md tells users to (deploy/compose.yaml
# and deploy/install.env), then checks the instance works: it starts, reports
# the expected version, accepts an administrator created with the CLI,
# captures a page of the bundled test site and finds it through the API.
#
#   scripts/smoke-install.sh IMAGE[:TAG] [EXPECTED_VERSION]
#
# Needs docker with the compose plugin, curl, jq and openssl. Everything runs
# in a temporary directory and a separate compose project, removed at exit.
set -euo pipefail

image=${1:?usage: smoke-install.sh IMAGE[:TAG] [EXPECTED_VERSION]}
want_version=${2:-}
repo=$(cd "$(dirname "$0")/.." && pwd)
dir=$(mktemp -d)
project=hoarfrost-smoke-$$
port=${SMOKE_PORT:-18080}

cleanup() {
	status=$?
	if [ $status -ne 0 ]; then
		echo "--- logs" >&2
		(cd "$dir" && docker compose -p "$project" logs --no-color --tail 200) >&2 || true
	fi
	(cd "$dir" && docker compose -p "$project" down -v --remove-orphans) >/dev/null 2>&1 || true
	rm -rf "$dir"
	exit $status
}
trap cleanup EXIT

step() { echo "==> $*"; }

step "installing from deploy/compose.yaml into $dir"
cp "$repo/deploy/compose.yaml" "$dir/compose.yaml"
cp "$repo/deploy/install.env" "$dir/.env"
cd "$dir"
# The secret-filling one-liner from install.env, as users run it.
sed -i "s/^POSTGRES_PASSWORD=$/POSTGRES_PASSWORD=$(openssl rand -hex 24)/; s/^HOARFROST_SECRET=$/HOARFROST_SECRET=$(openssl rand -hex 32)/; s/^HOARFROST_WORKER_TOKEN=$/HOARFROST_WORKER_TOKEN=$(openssl rand -hex 24)/" .env
for v in POSTGRES_PASSWORD HOARFROST_SECRET HOARFROST_WORKER_TOKEN; do
	grep -Eq "^$v=.{32,}" .env || { echo "$v was not filled in" >&2; exit 1; }
done
name=${image%:*}
tag=latest
[ "$name" != "$image" ] && tag=${image##*:}
cat >>.env <<EOF
HOARFROST_IMAGE=$name
HOARFROST_VERSION=$tag
HOARFROST_PORT=$port
EOF
# The test site ships in the image; it only exists for this check.
cat >compose.override.yaml <<EOF
services:
  testsite:
    image: $image
    entrypoint: ["hoarfrost-testsite", "-addr", ":8000", "-base", "http://testsite:8000"]
EOF

step "starting"
docker compose -p "$project" up -d --quiet-pull
base=http://127.0.0.1:$port
for i in $(seq 90); do
	curl -fsS "$base/healthz" >/dev/null 2>&1 && break
	[ "$i" = 90 ] && { echo "no answer on $base/healthz" >&2; exit 1; }
	sleep 2
done

hf() { docker compose -p "$project" exec -T hoarfrost hoarfrost "$@"; }
got=$(hf version)
step "running $got"
if [ -n "$want_version" ] && [ "$got" != "hoarfrost/$want_version" ]; then
	echo "expected hoarfrost/$want_version" >&2
	exit 1
fi

step "creating the administrator"
echo "smoke-password-$(openssl rand -hex 8)" | hf user -role admin -password-stdin add admin
token=$(hf user token admin smoke | tr -d '\r')
api() { curl -fsS -H "Authorization: Bearer $token" "$@"; }
curl -fsS -o /dev/null "$base/login"

step "capturing a page of the test site"
api -X POST -H 'Content-Type: application/json' -d '{"url": "http://testsite:8000/news/article.html"}' "$base/api/urls" >/dev/null
for i in $(seq 90); do
	n=$(api "$base/api/pages?after=0&limit=10" | jq '.results | length')
	[ "$n" -gt 0 ] && break
	[ "$i" = 90 ] && { echo "the page was not captured" >&2; exit 1; }
	sleep 2
done
api "$base/api/pages?after=0&limit=10" | jq -e '.results[0].url | contains("/news/article.html")' >/dev/null
step "ok: installed, captured and found a page"
