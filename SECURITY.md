# Security policy

hoarfrost stores captured third-party content and serves it to logged-in
users, so it treats archived pages as untrusted: previews and downloads are
sandboxed with a Content-Security-Policy, secrets are kept out of the archive,
and passwords, tokens and API keys are stored only as hashes.

## Reporting a vulnerability

Please do not open a public issue for security problems. Report them
privately through GitHub's
[private vulnerability reporting](https://github.com/Ogefest/hoarfrost/security/advisories/new)
with a description, steps to reproduce and the affected version
(`hoarfrost version` or the commit).

## Supported versions

Fixes are made on the `main` branch; there are no maintained release
branches yet.

## Deployment hardening

See [Security notes](docs/operations.md#security-notes) in the operations
guide: put the coordinator behind HTTPS, keep `HOARFROST_SECRET` and
`HOARFROST_WORKER_TOKEN` out of version control and backups of the archive,
and give workers write access only to the storage they need.
