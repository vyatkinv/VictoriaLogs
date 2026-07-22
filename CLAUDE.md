# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build Commands

```bash
# Build single-node binary (output: bin/victoria-logs)
make victoria-logs

# Build with race detector
make victoria-logs-race

# Build all binaries (victoria-logs, vlagent, vlogscli)
make all

# Build for specific app
APP_NAME=vlagent make app-local
APP_NAME=vlogscli make app-local
```

## Testing

```bash
# Run all unit tests (required: -tags 'synctest' for proper fasttime behavior)
go test -tags 'synctest' ./lib/... ./app/...

# Run a single package's tests
go test -tags 'synctest' ./lib/logstorage/...

# Run a specific test
go test -tags 'synctest' -run TestFunctionName ./lib/logstorage/...

# Run tests with race detector
go test -tags 'synctest' -race ./lib/... ./app/...

# Run integration tests (builds binaries first)
make apptest

# Run benchmarks
go test -run=NO_TESTS -bench=. ./lib/...
```

## Linting & Formatting

```bash
# Format code
make fmt

# Run go vet
make vet

# Run golangci-lint (installs if missing)
make golangci-lint

# Run all checks
make check-all
```

## Code Review Instructions

- Only leave a comment if you are confident the issue is a real bug or a clear mistake.
- Avoid speculative or hypothetical issues.
- If you provide a summary, limit it to one sentence to avoid verbosity.
- Avoid using header markdown formatting in comments.

## Architecture

VictoriaLogs is a high-performance log database written in Go. It supports both single-node and cluster deployments.

### Applications (`app/`)

- **`victoria-logs/`** — Single-node entry point. Wires together `vlinsert`, `vlselect`, and `vlstorage` behind a single HTTP server on `:9428`.
- **`vlinsert/`** — HTTP handlers for all log ingestion protocols: Elasticsearch, Loki, OpenTelemetry, Datadog, Splunk, Syslog, Journald, and native JSON line format. Routes `/insert/*` and protocol-native paths.
- **`vlselect/`** — HTTP handlers for LogsQL query endpoints (`/select/logsql/*`), live tailing, stats queries, delete operations, and the embedded VMUI web interface. Enforces concurrency limits via a channel semaphore.
- **`vlstorage/`** — Wraps `lib/logstorage` with HTTP-level configuration (retention, data path, cluster routing via `-storageNode` flag). In cluster mode, routes inserts and selects to remote storage nodes via `netinsert`/`netselect` subpackages.
- **`vlagent/`** — Log collection agent with file-based (`filecollector`) and Kubernetes (`kubernetescollector`) log sources. Forwards collected logs to a remote VictoriaLogs instance.
- **`vlogscli/`** — Interactive CLI for querying VictoriaLogs using LogsQL, with syntax highlighting and pager support.
- **`vlogsgenerator/`** — Benchmark/test tool for generating synthetic log streams.

### Core Library (`lib/logstorage/`)

The entire storage engine, query execution, and LogsQL language implementation lives here (~340 files):

- **`storage.go` / `storage_search.go`** — Top-level `Storage` struct. Manages partitions (one per day), handles log ingestion (`RunQuery`, `GetFieldNames`, etc.), and deletion tasks.
- **`parser.go`** — LogsQL parser. Produces `Query` (filter + pipe chain) and `Filter` types. Entry points: `ParseQuery()`, `ParseFilter()`.
- **`filter_*.go`** — One file per LogsQL filter type (e.g., `filter_exact.go`, `filter_regexp.go`, `filter_range.go`). Each implements the `filter` interface for block-level search.
- **`pipe_*.go`** — One file per LogsQL pipe operator (e.g., `pipe_filter.go`, `pipe_sort.go`, `pipe_format.go`, `pipe_stats.go`). ~55 pipe types implement streaming transformations on `blockResult`.
- **`stats_*.go`** — One file per aggregation function used in `| stats` (e.g., `stats_count.go`, `stats_sum.go`, `stats_histogram.go`). ~25 stats functions.
- **`block_result.go`** — Central data structure passed between pipes during query execution. Holds column data for a batch of log rows.
- **`block_search.go`** — Applies filters to on-disk blocks; drives per-partition parallel search.
- **`datadb.go`** — Low-level block storage: writing, reading, and merging of data blocks within a partition.
- **`values_encoder.go`** — Columnar encoding for log field values (dict, const, uint, float, timestamp, IP, IPv6 encodings).
- **`tokenizer.go`** / **`bloomfilter.go`** — Token extraction and Bloom filter support for fast full-text search pre-filtering.

### Secondary Library (`lib/prefixfilter/`)

A small utility for allow/deny filtering by exact strings or glob prefixes (e.g., used by `hidden_fields_filters` to exclude fields from query results).

### Cluster Architecture

In cluster mode, a single `victoria-logs` binary can be split into roles:
- **vlinsert role** (`-select.disable`): accepts only ingestion traffic and forwards to storage nodes
- **vlstorage role** (`-insert.disable`, `-select.disable`): stores data locally
- **vlselect role** (`-insert.disable`): accepts only query traffic

The `-storageNode` flag on the combined or insert/select nodes points to storage node addresses. `vlstorage/netinsert` and `vlstorage/netselect` handle the inter-node protocol.

### Integration Tests (`apptest/`)

Tests in `apptest/tests/*_test.go` start real application binaries from `bin/` and exercise them end-to-end over HTTP. Requires `make all` (or `make apptest` which builds automatically) before running with `go test ./apptest/...`.

### Key Conventions

- The `synctest` build tag must be passed when running tests: `go test -tags 'synctest' ...`. This switches `lib/fasttime` to use real `time.Now()` instead of a cached value, which is required for test correctness.
- All HTTP metrics use the `vl_` prefix (e.g., `vl_http_requests_total`).
- Log rows are grouped into streams identified by a set of stream fields. Streams are indexed separately from log content for fast stream-based filtering.
- The special `_msg` field holds the log message body. Fields prefixed with `_` are generally internal/hidden fields.
- Vendor directory is committed (`vendor/`); update with `make vendor-update`. Never edit anything under `vendor/` — it breaks the update.
- `lib/httpserver`, `lib/writeconcurrencylimiter` and `lib/protoparser/protoparserutil` are verbatim **forks** of VictoriaMetrics packages, not project code. Keep them byte-identical to upstream and mark every deviation with a `VL-FORK:` comment. Run `scripts/upstream-fork-diff.sh` before `make vendor-update` to see what upstream changed. Rationale and the reconciliation procedure: `lib/httpserver/UPSTREAM.md`.
