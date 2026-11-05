# Datum Cloud Looking Glass

The Datum Cloud Looking Glass gives operators and customers a read-only view
into the Datum Cloud network. Use it to inspect routing state and run
diagnostic queries, such as route lookups and reachability probes, from Datum
Cloud locations.

## License

See [LICENSE](LICENSE).

## API

The alpha API and client contract is in
[docs/api/v1alpha-contract.md](docs/api/v1alpha-contract.md).

The Go types are in [api/v1alpha](api/v1alpha), and `task generate` builds
their CRDs into [config/crd](config/crd). Each kind is registered with Milo
IAM as a project resource in [config/iam](config/iam/protected-resources),
with the contract's roles in [config/iam/roles](config/iam/roles).
Schema and CEL validation reject malformed requests. The session admission
webhook ([internal/admission](internal/admission), manifests in
[config/webhook](config/webhook)) refuses creates that are outside a project,
over the project's create rate, unauthorized for the diagnostic, at a location
that does not offer it, or aimed at a special-use or platform destination.
[config/quota](config/quota) limits each project to 20 sessions. The manager
serves the webhook when `webhook.enabled` is set in its configuration.

## Development

Requirements: Go (the version in `go.mod`), [Task](https://taskfile.dev), and
for the local environment, Docker and [kind](https://kind.sigs.k8s.io).
Task installs the remaining tools (golangci-lint, controller-gen and the
envtest API server) into `bin/`.

| Command | What it does |
| --- | --- |
| `task build` | Build `bin/looking-glass` |
| `task test` | Run unit tests and the envtest integration tests |
| `task lint` | Run golangci-lint |
| `task generate` | Tidy modules and regenerate deep copy code and CRDs |
| `task verify` | Fail if `task generate` changes anything |
| `task ci` | Everything CI runs: verify, lint, build and test |
| `task image` | Build the container image |

The `looking-glass` binary has two subcommands:

- `manager` runs the controller manager. Pass `--config` to read a
  configuration file (see [config/dev/manager.yaml](config/dev/manager.yaml)
  and `internal/config` for the fields and defaults). It serves `/healthz` and
  `/readyz` on the health address and Prometheus metrics on the metrics
  address. On SIGTERM it stops its runnables within
  `gracefulShutdownTimeout`.
- `fake-backend` serves a fake of Galactic's
  `fabric.service.v1.LookingGlassService` over plaintext gRPC. It validates
  requests as the service does and streams synthetic results for the alpha
  diagnostics. Use `--unreachable` to make routers fail and exercise partial
  coverage. It never probes a network, so it is for local use only.

### Local environment

```sh
task dev:up     # create the looking-glass-dev kind cluster
task dev:run    # run the fake backend (127.0.0.1:9090) and the manager
task dev:down   # delete the cluster
```

`task dev:run` runs both processes on your machine, against the kind cluster,
and Ctrl-C stops both.
