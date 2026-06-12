# Agent Substrate

## Project Overview

Agent Substrate is a Kubernetes-based system for running agent-like workloads at higher scale, efficiency, and lower latency than Kubernetes alone can offer. It takes the Kubernetes control plane out of the critical path by multiplexing a larger set of "actors" (stateful agent instances) onto a smaller set of ready "workers" (Kubernetes Pods), using gVisor checkpoint/restore for sub-100ms activation. It relies on the fact that agent-like applications are idle most of the time to achieve heavy multiplexing. State (memory + disk snapshots) is persisted to GCS/S3 between suspensions.

For development, it's recommended to read the `README.md` and `CONTRIBUTING.md` in the root folder.
See `hack/install-ate.sh` and `tools/setup-gcp` for provisioning and deploying clusters and GCP resources.

## Repository Layout

```
cmd/          # One subdirectory per binary (ateapi, atelet, atenet, …)
internal/     # Shared packages, internal to this module only
pkg/          # Shared packages intended for external import
docs/         # Design docs and developer guides
hack/         # Dev/CI scripts and code generators
manifests/    # Kubernetes YAML for deploying Agent Substrate
demos/        # Self-contained example applications
benchmarking/ # Load-testing tools and workloads
tools/        # Standalone Go tools (go run ./tools/<name>) for Dev/CI
```

**Where to put new Go code — quick rules:**

| Situation | Location |
|---|---|
| Only used by one binary | `cmd/<binary>/internal/<pkg>` |
| Shared across binaries, not for external import | `internal/<pkg>` |
| Public API for external consumers | `pkg/<pkg>` (use sparingly — implies a compatibility guarantee) |
| Public proto (control-plane gRPC API) | `pkg/proto/<name>` |
| Internal proto (atelet / ateom) | `internal/proto/<name>` |
| Dev/CI scripts | `hack/` |
| Standalone Go dev/CI tools | `tools/<name>` with its own `go.mod` |

See `docs/dev/code-layout.md` for the full rationale and per-directory details.

## Build and Test Commands

Agent Substrate uses a `Makefile` for its build and test tasks.

```shell
make build            # Build container images (via ko) and kubectl-ate binary
make build-atectl     # Build only the kubectl-ate CLI → bin/kubectl-ate
make build-images     # Build container images via ko
make build-demos      # Build demo application images
make test             # Run all unit tests (go test ./...)
go test ./internal/rendezvous/...   # Run tests for a single package
make lint             # Run golangci-lint (config: .golangci.yaml)
make fmt              # Auto-format all Go files with gofmt
make verify           # Run all verifiers: tests + gofmt + boilerplate + licenses + go modules + shellcheck
make e2e              # Build everything then run end-to-end tests (requires a running cluster)
```

E2E suites live in `internal/e2e/suites/`. Run them directly for more control:

```shell
hack/run-e2e.sh -run TestExample              # Run a single suite against your current cluster
hack/run-e2e.sh -args -kube-context my-ctx    # Pass flags through to the E2E framework (after -args)
hack/run-e2e-kind.sh                          # Run E2E against a local kind cluster (used in CI)
```

Protobuf regeneration: `go generate ./pkg/proto/... ./internal/proto/...` (uses `hack/protoc.sh` to pin the protoc version).

Code generators (deepcopy, client-gen): `go generate ./pkg/api/...`

## Architecture

### System Components

- **ateapi** (`cmd/ateapi`): Control-plane gRPC server. Manages actor/worker lifecycle, scheduling, and state in ValKey/Redis.
- **atelet** (`cmd/atelet`): Node-level DaemonSet supervisor. Manages worker pods, coordinates snapshotting, streams snapshots to/from object storage.
- **ateom-gvisor** (`cmd/ateom-gvisor`): Runs inside worker pods. Provides a gRPC interface for `atelet` to trigger `runsc` checkpoint/restore operations.
- **atecontroller** (`cmd/atecontroller`): Kubernetes controller reconciling WorkerPool and ActorTemplate CRDs.
- **atenet** (`cmd/atenet`): Networking stack — DNS, Envoy-based routing with External Processing, proxy sidecars. Extracts actor ID from the `Host` header, triggers resume on demand.
- **podcertcontroller** (`cmd/podcertcontroller`): Issues pod TLS certificates (polyfill for an upstream Kubernetes feature).
- **kubectl-ate** (`cmd/kubectl-ate`): CLI plugin for managing Substrate resources.

### Two-Layer Resource Model

- **CRDs (Kubernetes)**: `WorkerPool` and `ActorTemplate` — declarative system configuration, managed via `pkg/api/v1alpha1/`.
- **Database (ValKey/Redis)**: `Actor` and `Worker` instance state — high-frequency, real-time state transitions for scheduling and routing.

### Actor Lifecycle

`CreateActor` (suspended) → `ResumeActor` (assign worker, restore snapshot, running) → `SuspendActor` (checkpoint, persist snapshot, reclaim worker, suspended) → Delete.

### Key Internal Packages

- `internal/ateclient`: Client for the ateapi control plane
- `internal/controllers`: Kubernetes controller logic for CRDs
- `internal/rendezvous`: Rendezvous hashing for worker assignment
- `internal/serverboot`: Common server bootstrap/startup utilities
- `internal/credbundle`: TLS credential bundle management
- `internal/dns`: DNS resolution for actor routing

### Proto / gRPC APIs

- `pkg/proto/ateapipb`: Public control-plane API (ateapi ↔ clients/atenet)
- `internal/proto/ateletpb`: Internal API (ateapi ↔ atelet)
- `internal/proto/ateompb`: Internal API (atelet ↔ ateom inside worker pods)

## Local Development Setup

```shell
# Kind cluster (local)
hack/create-kind-cluster.sh
hack/install-ate-kind.sh --deploy-ate-system
hack/install-ate-kind.sh --deploy-demo-counter
go install ./cmd/kubectl-ate

# GKE cluster
cp hack/ate-dev-env.sh.example .ate-dev-env.sh  # edit with your project
source .ate-dev-env.sh
go run ./tools/setup-gcp --all
hack/install-ate.sh --deploy-ate-system
```

Use `hack/install-ate-kind.sh --help` or `hack/install-ate.sh --help` for component-level deploy/delete options.

## Code Style Guidelines

- **Go Formatting**: Code must be formatted with `gofmt`. Run `make fmt` to automatically format all files before submitting changes.
- **Copyright Headers**: Every source file must include a copyright and license header. Templates are in `hack/boilerplate/`; `make verify` checks this automatically.
- **Modularity**: Submit small, focused Pull Requests that touch a limited part of the codebase for easier reviews and rebasing.
- **Go Modules**: Keep `go.mod` clean. Run `go mod tidy` if adding or removing dependencies.

## Testing Instructions

1. Write tests for all new code. We will not merge code that lacks tests.
2. Ensure changes do not break existing tests.
3. Run `make verify` locally before requesting a code review to catch common issues like missed copyright headers or formatting drift.
4. For end-to-end tests involving the actual infrastructure, ensure you have a running cluster (setup via `hack/ate-dev-env.sh.example` and `go run ./tools/setup-gcp --all`).

## Security Considerations

The security story for Substrate is very early and many features are missing.
However! Take care to respect security best practices when writing code in order to improve Substrate's security over time.
The following is what Substrate currently offers.
Keep this up to date when updating AGENTS.md.

- **Workload Isolation**: The project uses `gVisor` (`runsc`) for sandboxing and security isolation of workloads on pods. A temporary gVisor patch might be required (check the README instructions).

For future plans for security, reference `docs/roadmap.md`.

## Reference Docs

In-repo design docs and guides (read these before deep work in the relevant area):

- `docs/architecture.md` — system architecture and component interactions
- `docs/api-guide.md` — control-plane API usage
- `docs/observability.md` — metrics, logging, tracing
- `docs/dev/code-layout.md` — full rationale for the code placement rules above
- `docs/dev/valkey-direct-access.md` — inspecting/debugging ValKey state
- `docs/roadmap.md` — planned work, including the security roadmap
