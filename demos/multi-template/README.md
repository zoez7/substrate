# Multi-Template Demo

This demo shows that **two different `ActorTemplate`s running two different binaries
can share a single `WorkerPool`**.

The templates are substrate resources living in their own atespaces, entirely
decoupled from the pool's Kubernetes namespace. Each one gates on the pool via
`workerSelector`, a label selector matched against the pool's labels — pool
selection is cluster-wide.

## Prerequisites

- A k8s cluster with Agent Substrate installed (`./hack/install-ate.sh --deploy-ate-system`).
- `ko` installed for building images.
- A GCS bucket for storing snapshots (configured via `BUCKET_NAME` env var).

## How to Run on Agent Substrate

### 1. Build and Deploy

> [!NOTE]
> Do not manually edit the `demos/multi-template/*.yaml.tmpl` manifests. The installation
> script automatically injects your `${BUCKET_NAME}` environment variable during deployment.

```bash
./hack/install-ate.sh --deploy-demo-multi-template
```

This command will:
- Build the `counter` and `fspersist` images using `ko`.
- Create one `WorkerPool` (`shared-pool`) in the `ate-demo-multi-template-pool`
  namespace and wait for its rollout.
- Create two atespaces — `ate-demo-multi-template-counter` and
  `ate-demo-multi-template-fspersist` — and one actor template in each through
  the ate API (`kubectl ate create actor-template`), both selecting the pool
  via the same `workerSelector` label. No Kubernetes namespace backs the
  atespaces: templates are substrate resources, not CRDs.
- Wait until both templates have their golden snapshots built.

### 2. Create one actor per template

Each actor is created in its template's atespace: `--template-ref` resolves
the template by name within the actor's own atespace, and the deploy step
already created both atespaces.

```bash
# Install the CLI as a kubectl plugin if not already installed
go install ./cmd/kubectl-ate

# Create one actor from each template, in that template's atespace.
kubectl ate create actor c1 -a ate-demo-multi-template-counter --template-ref counter
kubectl ate create actor f1 -a ate-demo-multi-template-fspersist --template-ref fspersist
```

### 3. Port-forward the atenet router

To interact with the router locally:

```bash
kubectl port-forward -n ate-system svc/atenet-router 8000:80
```

## How to Use

When you send an HTTP request through the router, Substrate automatically detects the session, activates (resumes) the actor onto an available worker pod, and proxies the traffic.

```bash
# counter binary
curl -s -H "Host: c1.ate-demo-multi-template-counter.actors.resources.substrate.ate.dev" http://localhost:8000
# -> hello from: <ip> | preserved memory count: 1

# fspersist binary
curl -s -H "Host: f1.ate-demo-multi-template-fspersist.actors.resources.substrate.ate.dev" http://localhost:8000
# -> pod: <ip>
#    --- history ---
#    pod=<ip> | count=0 | time=<timestamp>
```

Confirm both actors landed on workers in the one `shared-pool`:

```bash
kubectl ate get workers
```

The `counter` increments its in-memory count on each request, while `fspersist` prepends
a line to its history file on each request. Suspending and re-requesting an actor
preserves that state across the snapshot/restore cycle:

```bash
kubectl ate suspend actor f1 -a ate-demo-multi-template-fspersist
curl -s -H "Host: f1.ate-demo-multi-template-fspersist.actors.resources.substrate.ate.dev" http://localhost:8000  # history persists; count keeps climbing
```

## How to Uninstall

Delete the actors:

```bash
# For example:
kubectl ate delete actor c1 -a ate-demo-multi-template-counter
kubectl ate delete actor f1 -a ate-demo-multi-template-fspersist
```

Then remove the templates, their atespaces, and the pool:

```bash
./hack/install-ate.sh --delete-demo-multi-template
```
