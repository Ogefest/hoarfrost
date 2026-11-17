# Installing hoarfrost

hoarfrost is released as a container image (`ghcr.io/ogefest/hoarfrost`,
linux/amd64 and linux/arm64) and as binaries for Linux and macOS. Every
release is listed on the [releases page](https://github.com/Ogefest/hoarfrost/releases)
with its changes and upgrade notes.

The quickest way to a working instance is Docker Compose with the bundled
PostgreSQL. It takes about five minutes.

## With Docker Compose (recommended)

You need Docker with the Compose plugin (`docker compose version`) and
disk space for the archive.

1. **Download the two files of the latest release** into an empty directory:

   ```sh
   mkdir hoarfrost && cd hoarfrost
   curl -LO https://github.com/Ogefest/hoarfrost/releases/latest/download/compose.yaml
   curl -L -o .env https://github.com/Ogefest/hoarfrost/releases/latest/download/install.env
   ```

   `install.env` is pinned to the version it was released with.

2. **Fill in the secrets** in `.env`: the database password, the password
   pepper and the worker token. This command puts random values in all
   three:

   ```sh
   sed -i "s/^POSTGRES_PASSWORD=$/POSTGRES_PASSWORD=$(openssl rand -hex 24)/; s/^HOARFROST_SECRET=$/HOARFROST_SECRET=$(openssl rand -hex 32)/; s/^HOARFROST_WORKER_TOKEN=$/HOARFROST_WORKER_TOKEN=$(openssl rand -hex 24)/" .env
   ```

   On macOS use `sed -i ''`. Keep a copy of `.env` somewhere safe (see
   [Backups](#backups)).

3. **Decide where the archive goes.** By default it is a Docker volume. To
   keep the WARC/WACZ files in a directory you can see and back up, set
   `HOARFROST_ARCHIVE_DIR` in `.env`; for S3-compatible storage set
   `HOARFROST_STORAGE` and the credentials. The comments in `.env` explain
   both.

4. **Start it:**

   ```sh
   docker compose up -d
   docker compose logs -f hoarfrost     # Ctrl-C to stop following
   ```

5. **Open <http://localhost:8080>**, create the administrator account and
   add a first source on the **Sources** page: an RSS feed, a sitemap or a
   list of URLs. The **Dashboard** shows captures within a minute or two.

### Reaching it from other machines

The UI listens on `127.0.0.1` only. To use it from elsewhere, put it behind
an HTTPS reverse proxy (Caddy, nginx, Traefik) pointing at port 8080. For
example, with Caddy:

```
archive.example.org {
	reverse_proxy 127.0.0.1:8080
}
```

On a trusted network without HTTPS, set `HOARFROST_BIND=0.0.0.0` and
`HOARFROST_INSECURE_COOKIE=true` in `.env` and run `docker compose up -d`
again. Login cookies otherwise need HTTPS.

### Checking that it works

```sh
docker compose ps                                  # both services running
curl -s http://localhost:8080/healthz              # "ok"
docker compose exec hoarfrost hoarfrost version    # the release you installed
```

The version is also shown in the footer of every page.

## Upgrading

1. Read the release notes of every version between yours and the new one.
   Breaking changes and database migrations have their own sections.
2. Back up the database (below).
3. Set the new version in `.env` and restart:

   ```sh
   sed -i 's/^HOARFROST_VERSION=.*/HOARFROST_VERSION=0.2.0/' .env
   docker compose pull && docker compose up -d
   ```

Migrations run when the new version starts. When a release changes the
layout of the search index, the index is rebuilt from the archive after the
start, which takes a while on a large archive: the web UI works meanwhile and
shows the progress, but search may miss weeks not rebuilt yet. The archive
itself is never rewritten. Going back to an older
version after a migration needs the database backup.

`HOARFROST_VERSION=latest` follows every release automatically on
`docker compose pull`. Pinning a version and changing it on purpose is
safer.

## Backups

What to keep, in order of importance:

1. **The archive** (the directory or bucket in `HOARFROST_STORAGE`). It is
   the source of truth and can be read without hoarfrost. Copy it with any
   tool (`rsync`, `rclone`).
2. **`.env`**, above all `HOARFROST_SECRET`. Without it no one can log in
   until passwords are reset from the command line.
3. **The database schema `app`** (users, sources, queue):

   ```sh
   docker compose exec -T postgres pg_dump -U hoarfrost -n app hoarfrost > app-$(date +%F).sql
   ```

   The search index (schema `archive_index`) is rebuilt from the archive and
   needs no backup.

See [operations.md](operations.md#backups-and-recovery) for recovery.

## Other ways to install

### Binary

Download `hoarfrost_<version>_<os>_<arch>.tar.gz` from the release and check
it against `SHA256SUMS`. The binary needs PostgreSQL (tested on 17);
Chromium (`chrome` engine), `pdftotext` (PDF text) and `tesseract`
(OCR) are optional.

```sh
tar xzf hoarfrost_*_linux_amd64.tar.gz && cd hoarfrost_*_linux_amd64
cp .env.example .env     # set HOARFROST_DATABASE, HOARFROST_SECRET, HOARFROST_STORAGE
./hoarfrost serve
```

[operations.md](operations.md#running) covers running it as a service and
adding workers.

### From source

```sh
git clone https://github.com/Ogefest/hoarfrost && cd hoarfrost
make build               # needs Go (version in go.mod)
```

`docker compose up --build` in the repository starts a development setup
with a test news site, see [CONTRIBUTING.md](../CONTRIBUTING.md).

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `HOARFROST_SECRET must be set to at least 32 random characters` | step 2 was skipped; fill in `.env` and `docker compose up -d` |
| `set POSTGRES_PASSWORD in .env` when starting | same |
| Login page reloads without logging in | plain HTTP from another machine; use HTTPS or set `HOARFROST_INSECURE_COOKIE=true` |
| `permission denied` writing to the archive directory | the directory must be writable by the image user: `sudo chown "$(docker compose run --rm hoarfrost id -u)" <dir>` |
| Pages stay queued | check the Dashboard: workers online and slots busy; raise `HOARFROST_WORKERS` if every slot is busy |
| Database password changed in `.env` after the first start | PostgreSQL keeps the original password; put the old one back or change it inside PostgreSQL |

Still stuck? [Open an issue](https://github.com/Ogefest/hoarfrost/issues/new/choose)
with the version and the relevant log lines.
