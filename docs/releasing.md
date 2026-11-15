# Development flow and releases

## Day to day

- `main` is always releasable: CI is green on every commit.
- Every change is a pull request from its own branch off an up-to-date
  `main`, merged when CI passes.
- Every pull request gets a label, which decides its section of the release
  notes (`.github/release.yml`):

  | Label | Section | Use for |
  |---|---|---|
  | `breaking` | Breaking changes | users must act on upgrade (changed setting, removed endpoint); say what in the PR description |
  | `migration` | Database migrations | a new migration in `internal/appdb` or a new `schemaVersion` of the index (which triggers a rebuild) |
  | `enhancement` | New features and improvements | |
  | `bug` | Fixes | |
  | `documentation` | Documentation | |
  | `skip-changelog` | (left out) | CI tweaks, refactoring nobody notices |

  A PR may have several (`enhancement` + `migration`).

Merged pull requests accumulate on `main` until the next release.

## CI

`.github/workflows/ci.yml` runs on pull requests and on `main`: unit and
database tests with the race detector, lint, fuzzing, Prometheus rules,
end-to-end and conformance tests with independent tools (warcio, py-wacz,
pywb), the S3 contract, and an **install test**: the image is built and
installed exactly as [install.md](install.md) describes
(`scripts/smoke-install.sh`). It then has to capture a page of the bundled
test site and return it through the API.

Run the install test locally against any image:

```sh
docker build -t hoarfrost:dev . && scripts/smoke-install.sh hoarfrost:dev
```

## Cutting a release

Versions follow [semver](https://semver.org): `vMAJOR.MINOR.PATCH`. Before
1.0, a minor bump (`0.3.0`) may contain breaking changes, which are always
listed in the notes. A patch bump (`0.3.1`) contains fixes only.

1. Look at what is unreleased:

   ```sh
   git fetch --tags
   git log --oneline $(git describe --tags --abbrev=0)..origin/main
   gh pr list --state merged --search "merged:>$(git log -1 --format=%cs $(git describe --tags --abbrev=0))"
   ```

   Check that every merged PR has a label.

2. Tag `main` with an annotated tag. The body of the tag message (after the
   first line) becomes the **Upgrade notes** section of the release. Write it whenever an upgrade needs more
   than `docker compose pull && docker compose up -d`: a long migration, an
   index rebuild, a changed setting. Otherwise give only the first line.

   ```sh
   git switch main && git pull
   git tag -a v0.2.0 -m "v0.2.0" -m "Adds an index on jobs(finished); on a queue of millions of jobs the first start takes a few extra seconds."
   git push origin v0.2.0
   ```

3. `.github/workflows/release.yml` takes over:
   1. the whole CI suite;
   2. binaries for linux/darwin × amd64/arm64 with `SHA256SUMS`, plus
      `compose.yaml` and `install.env` (pinned to the version);
   3. the image is built, put through the install test with the expected
      version, then pushed as `ghcr.io/ogefest/hoarfrost:0.2.0`, `:0.2` and
      `:latest`;
   4. a GitHub release with install instructions, the upgrade notes and the
      generated, grouped list of changes.

   Follow it with `gh run watch`. If it fails, nothing is published except
   possibly the image. Fix the problem on `main`, delete the tag
   (`git push --delete origin v0.2.0 && git tag -d v0.2.0`) and tag again.

4. Check the release page and try the install commands from it.

To test changes to the release pipeline, push a pre-release tag such as
`v0.2.0-rc.1`. It is published as a pre-release and does not move `:latest`
or `:0.2`.

## The container image

Images live in the GitHub Container Registry as
`ghcr.io/ogefest/hoarfrost`, linked to this public repository, so anyone
can pull them without logging in.
