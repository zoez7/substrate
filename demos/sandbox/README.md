# Sandbox Demo

This directory contains a demo of a stateful sandbox execution environment running on Agent Substrate.

It allows you to run arbitrary commands in an sandboxed, isolated container (running Alpine Linux) and preserves the execution state across suspends and resumes.

> [!WARNING]
> **Security Disclaimer:** This demo is not secured by any authorization checks, and the sandbox actor will execute any client-provided commands with no validation. Do not deploy this configuration in production or expose it to untrusted networks.

## Components

1.  **Sandbox Server (`main.go`)**: The application that runs inside the Agent Substrate actor. It exposes a simple, stateless `/process` endpoint to execute commands.
2.  **Sandbox Client (`client/`)**: A CLI REPL tool that allows you to interact with the sandbox actor interactively.

## Prerequisites

- A k8s cluster with Agent Substrate installed.
- `ko` installed for building images.
- A GCS bucket for storing snapshots (configured in `demos/sandbox/sandbox.yaml.tmpl`).
- `kubectl-ate` CLI installed (can be installed via `go install ./cmd/kubectl-ate`).

## How to Run on Agent Substrate

### 1. Build and Deploy

> [!NOTE]
> Do not manually edit `demos/sandbox/sandbox.yaml.tmpl`. The installation script automatically injects your `${BUCKET_NAME}` environment variable during deployment.

Use the core installation script to build the image and apply the resolved manifests to your cluster:

```bash
./hack/install-ate.sh --deploy-demo-sandbox
```

This command will:
- Build the sandbox server image based on Alpine Linux.
- Create the `ate-demo-sandbox` namespace and the `WorkerPool`.
- Create the `ate-demo-sandbox` atespace and the `sandbox-template` actor
  template through the ate API (`kubectl ate create actor-template`), a
  substrate resource rather than a CRD.

The install does not block on the template's golden snapshot (the sandbox
client exercises the template on demand); check it with:
```bash
kubectl ate get actor-template sandbox-template -a ate-demo-sandbox
```

### 2. Create a Sandbox Actor

Create the sandbox actor with a chosen name (e.g., `my-sandbox-1`) in the
demo's atespace, which the deploy step already created (`--template-ref`
resolves the template by name within the actor's own atespace):

```bash
# Install the CLI as a kubectl plugin if not already installed
go install ./cmd/kubectl-ate

# Create the actor from the sandbox template.
kubectl ate create actor my-sandbox-1 -a ate-demo-sandbox --template-ref sandbox-template
```

### 3. Port-Forward Services

If running clients locally, port-forward the API and router in separate terminals:

```bash
# Terminal 1: API Server
kubectl port-forward -n ate-system svc/api 8080:443

# Terminal 2: Router
kubectl port-forward -n ate-system svc/atenet-router 8000:80
```

## How to Use the Client

Build and run the client REPL:

```bash
go build -o bin/sandbox-client ./demos/sandbox/client

./bin/sandbox-client --ateapi=localhost:8080 --atenet=localhost:8000 --atespace=ate-demo-sandbox --name=my-sandbox-1
```

Once in the `sandbox>` prompt, you can run commands:

```bash
sandbox> ls -la
sandbox> pwd
sandbox> echo "Hello" > test.txt
sandbox> cat test.txt
```

Type `exit` to leave. This will automatically trigger the suspension of the actor.

To permanently delete the suspended actor:
```bash
kubectl ate delete actor my-sandbox-1 -a ate-demo-sandbox
```

## How to Uninstall

To remove the sandbox demo resources (namespace, workerpool, and template) from your cluster, run:

```bash
./hack/install-ate.sh --delete-demo-sandbox
```
