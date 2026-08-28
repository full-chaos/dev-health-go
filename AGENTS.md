# AGENTS.md — dev-health-go

Shared Go store/contract library extracted from `acr` (CHAOS-4377, Wave 0 of
the Go API epic, CHAOS-4352). Consumers: `acr` and the new `query-api`.

## Scope

Store/contract layer ONLY:

- `clickhouse/` — read-only ClickHouse query client + statement guards.
- `schema/` — declared ClickHouse column-type contracts (`devhealthschema`).
- `readers/` — neutral, column-level ClickHouse row readers over `schema`
  types. Return plain structs. No GraphQL, no fact/resolver shapes.
- `contracts/` — store-level validation/versioning primitives.
- `authverify/` — JWKS, k8s TokenReview, and workload-token-exchange
  verification MECHANISMS. No principal/claim shape — each consumer defines
  its own on top.

**Never add:** resolver or response shapes, GraphQL types, a consumer
principal type, or anything that only makes sense with GraphQL/MCP
knowledge. If `query-api` would need GraphQL knowledge to use a symbol, it
does not belong here.

## Conventions

Mirrors `acr`'s Go conventions (see `acr/AGENTS.md`): `gofmt`, `go vet`,
table-driven tests beside packages, typed sentinel errors, no raw transport
errors leaked to callers. Every exported package doc explains WHY, not just
what — copy the reasoning comments across from `acr` verbatim; do not drop
them during extraction.

## Gates

```bash
make fmt-check
make vet
make test
```

CI (`.github/workflows/ci.yml`) runs the same on every PR and push to
`main`.

## Versioning

Consumers pin a tagged version (`go get github.com/full-chaos/dev-health-go@vX.Y.Z`).
A `replace` directive to a local path is for development only and must not
land in a merged PR.
