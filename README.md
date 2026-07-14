# jind-ai-plugin-registry

Registry of [jind-ai](https://github.com/takaaki-s/jind-ai) plugins.

## Consuming

The registry is served at:

```
https://takaaki-s.github.io/jind-ai-plugin-registry/registry.json
```

`jin plugin ls-remote` and `jin plugin install <name>` read this URL.

## Getting listed

Add the topic `jind-ai-plugin` to a public GitHub repository that contains a
valid `jind-ai-plugin.yaml` at the repo root. The next crawler run (every 6
hours, or on manual dispatch) will pick it up.

Manifest format and validation rules are documented in the
[jind-ai plugin registry docs](https://github.com/takaaki-s/jind-ai/blob/main/docs/plugin-registry.md).

## Name collisions

A plugin `name` is unique across the registry. Ownership is decided by:

1. If a name is already registered, its current owner keeps it.
2. If a new name is claimed by a single repo, that repo gets it.
3. If a new name is claimed by multiple repos in the same crawl, the
   repository with the earlier `created_at` wins.
4. If the owner repo disappears (deleted, archived, or the topic is removed),
   the entry is flagged with `orphaned_since` and kept for a 30-day grace
   period, after which the name is released.

## Local development

The crawler is a Go program under `cmd/crawler`. To run it locally against a
fresh state:

```
echo '{}' > /tmp/prev.json
GITHUB_TOKEN=$(gh auth token) \
    go run ./cmd/crawler --prev /tmp/prev.json --out /tmp/registry.json
```

Tests use `httptest.Server` mocks and do not hit GitHub:

```
go test ./...
```

The crawler imports [`pkg/plugin/manifest`](https://github.com/takaaki-s/jind-ai/tree/main/pkg/plugin/manifest)
from `github.com/takaaki-s/jind-ai` so validation is bit-for-bit identical to
`jin plugin validate`. During pre-1.0 development it is pinned to a
main-HEAD pseudo-version; once jin ships a plugin-registry release tag,
`go get github.com/takaaki-s/jind-ai@<tag>` bumps the dependency.

If you are working on unpublished jin changes locally, add a `go.work` at
the parent of both checkouts:

```
go 1.24.5

use (
    ./jind-ai
    ./jind-ai-plugin-registry
)
```

`go.work` is ignored by git so it does not leak into CI.
