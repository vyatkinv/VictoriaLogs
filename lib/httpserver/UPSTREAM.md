# Forked packages from VictoriaMetrics

`lib/httpserver`, `lib/writeconcurrencylimiter` and `lib/protoparser/protoparserutil`
are verbatim forks of the corresponding packages from
`github.com/VictoriaMetrics/VictoriaMetrics`.

## Why they are forked

`httpserver.Serve` builds the `tls.Config` for the HTTPS listener internally and
only accepts certificate **file paths**. `lib/vaulttls` needs to serve
Vault PKI certificates straight from memory, the same way the syslog listener
already does, which requires an injection point in `ServeOptions`.

The two other packages are forked because they are the only ones that pull the
vendored `httpserver` into the binary:

```
app/vlinsert/*  ->  protoparser/protoparserutil  ->  writeconcurrencylimiter  ->  httpserver
```

As long as any of them is linked, its `init()` registers the same flag names as
the fork (`-tls`, `-tlsCertFile`, `-http.*`, `-httpAuth.*`, `-maxConcurrentInserts`, ...)
and the process dies at startup with a duplicate flag registration panic —
at runtime, not at compile time.

`lib/flagutil` is intentionally **not** forked. `flagutil/secret.go` keeps a
package-level `secretFlags` registry that is populated by the vendored
`lib/envflag` and `lib/pushmetrics` and read by the `/flags` route via
`IsSecretFlag`. A second copy of that registry would make `/flags` print
secrets such as `-pushmetrics.url` in plain text.

## Local changes

Every deviation from upstream is marked with a `VL-FORK:` comment. Keep the
number of such markers minimal — everything else must stay byte-identical so the
fork can be reconciled mechanically on upstream updates.

Current local changes:

- `httpserver.go` — `ServeOptions.GetTLSConfig` field and the corresponding
  branch in `serve()`, used by `lib/vaulttls` to serve in-memory certificates.
- import paths rewritten from `.../VictoriaMetrics/lib/...` to
  `.../VictoriaLogs/lib/...` in `writeconcurrencylimiter/concurrencylimiter.go`
  and `protoparser/protoparserutil/compress_reader.go`.

## Upstream baseline

Module: `github.com/VictoriaMetrics/VictoriaMetrics`
Version: `v1.146.1-0.20260630165203-c82127b6d4d1`

sha256 of the upstream files at the moment of the fork (before the `VL-FORK:`
changes listed above were applied):

```
4822d71d4eaedb6616850652de12c9ab2ef86d655738bbd9b826481dee7339fe  httpserver/favicon.ico
39cbe61b6d3dbabf40f36717358006e568389d0cb05e1168f37a2adfe7724c76  httpserver/httpserver.go
037b88e367324fc79f6e7acd99fcbb79f1345ba0737ade77821767435872f644  httpserver/httpserver_test.go
ce0d6e31502b15c46b55e1b72c286492f5b814d22e0c241e86798dab0c117126  httpserver/path.go
3fd187c0812428d43f448571ca287bb1a22f7559a9b2c8b95bfba33c9a7ab11b  httpserver/path_test.go
7b1bfe0164f1a3f00a89e6d22d96d6e883e55b9c6ffe374205e2ed73eca03823  httpserver/prometheus_error_response.qtpl
6e1eb08e61e3c46f6bdb512621c4789fd4e934ac53faef334ef71ea4c2f55a72  httpserver/prometheus_error_response.qtpl.go
ca4996920722cc8eb389002b721c63d94ce7862e4e9a7c9a8cb2d5cfd0e6f002  httpserver/prometheus.go
82352ad4ba190edb10a047101fd7195500c71ad02aadf014a7fe5406277c0714  protoparser/protoparserutil/compress_reader.go
dfcea600967905e6de1243961e7005064eb9006c17d8bac8d0194c9ffc04d87a  protoparser/protoparserutil/compress_reader_test.go
90feb31929973c5914c95a5d632c99a06a49140f8c9b1bb3ab4cd239375c8ca3  protoparser/protoparserutil/extra_labels.go
d965c8212765720b27f3edb1dfdf8a3acade02015da7953bebf0dac424d96766  protoparser/protoparserutil/extra_labels_test.go
8cd3ee3d58189539014c101b2e773923c83cb5bc31b912889e9bf7c27f65d86a  protoparser/protoparserutil/lines_reader.go
9709493de9516e6e5a2e4e78db66cd81fc3733cb7acf1c926a2e270a0cf4bc61  protoparser/protoparserutil/lines_reader_test.go
6f2f99173fcf18b1d5287b8b26f8e30b4819ee2d81ca75a8ad8ed895dac16be5  protoparser/protoparserutil/timestamp.go
78259965aa8e05c3fa2116668c5484f3485a0274d222828ba27e732da79836e2  protoparser/protoparserutil/unmarshal_work.go
8b006b0cfe76cd85db28e701e18e31efc643f062087eed2b2fef8d2798413b81  protoparser/protoparserutil/vmproto_handshake.go
7b8c382b53c622f7583525147675fd24fc053c3c226e2071a163db3ff95a47d0  writeconcurrencylimiter/concurrencylimiter.go
```

## Reconciling with upstream

Run `scripts/upstream-fork-diff.sh` before every `make vendor-update`. It fetches
the version recorded above plus the version currently required by `go.mod`, and
shows what changed upstream in the forked packages.

The script needs network access and is deliberately **not** wired into
`make check-all`, because CI builds run air-gapped in vendor mode.
