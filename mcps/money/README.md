# Miser — Multi-Account Money MCP Server

> **This file is a pointer, not documentation.** It used to describe the tool
> surface and schema, drifted out of date, and contradicted the tools twice
> (paise units, then categories). The canonical references are:
>
> - `documentation.md` → Storage/Schema tables (collections, surface keys,
>   `money_categories`, retention);
> - the tool `Description`s in `cmd/miser-money/` (the contract callers see,
>   pinned by `cmd/miser-money/schema_test.go`);
> - `internal/money/` comments (store semantics, pinned by `go test`).
>
> If you need the old text, it is in git history (`git log -- mcps/money/README.md`).

## Run

```bash
./scripts/build-mcps.sh   # builds bin/miser-money (and the other servers)
```

T3/opencode launches it via `./scripts/mcp-run.sh miser-money` (see
`opencode.json`); the launcher fills `MONGODB_URI`/`MONGODB_DB` from the root
`.env` and refuses a binary older than its sources.
