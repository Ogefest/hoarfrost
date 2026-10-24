# Contributing

Thanks for helping! A few rules keep the archive trustworthy for decades:

1. **The archive in storage is the only source of truth.** Anything needed to
   read the archive must be in storage, in standard formats. The index
   (schema `archive_index`) must stay rebuildable (`internal/archive` reproducibility tests).
2. **Objects are immutable.** New data means new objects. Only `README.md` and
   `catalog/*.json` are rewritten.
3. **Standard formats only.** Custom metadata goes into `warcinfo`,
   `metadata` records or extra JSON fields.
4. **Every layer behind an interface** (storage, engines, recorder, index).
   New storage backends must pass `internal/storage/storagetest`.

## Workflow

```sh
make test race lint
make fuzz        # when touching parsers
make e2e         # end-to-end; install tools with `make conformance-tools`
```

Golden files of the static engine are regenerated with
`go test ./internal/engine/static -update`; review the diff.

Code style: `gofmt`/`goimports`, small packages, errors wrapped with
context, comments only where the code does not explain itself.

## Database schema changes

- Schema `app` holds data that cannot be rebuilt (users, tokens, queue):
  never edit an existing migration in `internal/appdb`, append a new one.
- Schema `archive_index` is derived data: change the schema in
  `internal/index` and bump `schemaVersion`; an index with another version is
  dropped on start and rebuilt from storage.
- Tests need a PostgreSQL database for scratch schemas:
  `HOARFROST_TEST_DATABASE=postgres://user:password@host/hoarfrost_test`
  (the user must be allowed to create databases for the end-to-end test).
  Without it database tests are skipped.
