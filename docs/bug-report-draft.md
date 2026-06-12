# Bug report (draft) — intermittent `503 connection termination` on cold-start resume

## Expected Behavior

A request to a suspended actor triggers a cold-start resume. Once `ResumeActor` reports the actor
as `STATUS_RUNNING` and ext_proc routes the request to `workerIP:80`, the request should be served
by the actor and return its normal `200` response — every time, not just most of the time.

## Actual Behavior

Intermittently (~1 in 8–10 cold starts), the client instead gets:

```
HTTP 503
upstream connect error or disconnect/reset before headers. reset reason: connection termination
```

This is an Envoy **data-plane** error (access-log flag `UC` = upstream connection termination), not
a control-plane error — the control plane succeeded and routed correctly:

- ext_proc logs `ResumeActor result … status:STATUS_RUNNING … err:null` then `Route ok …
  targetAddr:<workerIP>:80`.
- Envoy logs `"GET /" 503 UC … "<workerIP>:80"` at virtually the same millisecond the worker's
  `ateom` logs `RestoreWorkload` → `Actor restored`.

**Root cause:** the resume workflow marks the actor `STATUS_RUNNING` the instant `runsc restore`
detaches, without probing that the actor is accepting on `:80`
(`cmd/ateapi/internal/controlapi/workflow_resume.go:225`, `:285`). If Envoy connects in the brief
window before the freshly-restored gVisor sandbox is listening on `:80`, the connection is reset.
Because the Envoy route has no `retry_policy` (`cmd/atenet/internal/app/router/xds.go:270-297`),
that single transient reset is surfaced directly to the client instead of being retried.

It is **not** a "no free workers" case (that returns a different, clean body) and **not** a stale
worker IP (the same worker IP serves other requests with `200` immediately before/after).

## Steps to Reproduce the Problem

1. Install the multi-template demo and create the `fspersist` actor `f1` (it takes the snapshot
   restore path):
   ```bash
   ./hack/install-ate.sh --deploy-demo-multi-template
   kubectl ate create actor f1 --template ate-demo-multi-template-fspersist/fspersist
   kubectl port-forward -n ate-system svc/atenet-router 8000:80   # leave running
   ```
1. Force repeated cold starts by suspending the actor and immediately curling it:
   ```bash
   for round in $(seq 1 20); do
     kubectl ate suspend actor f1 >/dev/null 2>&1
     code=$(curl -s -o /tmp/body -w "%{http_code}" --max-time 12 \
       -H "Host: f1.actors.resources.substrate.ate.dev" http://localhost:8000)
     echo "round $round: HTTP $code | $(head -c 90 /tmp/body | tr '\n' ' ')"
   done
   ```
1. Observe that ~1 in 8–10 rounds returns `HTTP 503 … reset reason: connection termination`
   while the rest return `200`. Confirm it is a data-plane race (not control plane) by correlating
   the failing request across components:
   ```bash
   # control plane succeeded + routed:
   kubectl logs -n ate-system deploy/atenet-router -c atenet-router --tail=400 | grep -E "Route ok|ResumeActor result"
   # Envoy upstream reset (flag UC):
   kubectl logs -n ate-system deploy/atenet-router -c envoy --tail=500 | grep -E "\" 503 |UC"
   # worker restore timing ≈ Envoy reset timestamp:
   kubectl logs -n ate-demo-multi-template-pool <worker-pod> --tail=300 | grep -E "RestoreWorkload|Actor restored"
   ```
   (See `docs/repro.md` for the full reproduction + observation runbook.)

## Specifications

- Version: `multiactor-demo` @ `5170b7c` ("removed .gitignore in the fspersist folder"); behaviour
  unchanged on `main`. Relevant code: `workflow_resume.go` (no `:80` readiness gate before
  `STATUS_RUNNING`), `xds.go` (Envoy route has no `retry_policy`).
- Platform: GKE, Kubernetes v1.35 (us-central1-c); gVisor `runsc` nightly `2026-05-19`;
  worker nodes `c3-standard-4`, `linux/amd64`. Reproduces on the snapshot **restore** path
  (`fspersist`/`f1`); the fresh-boot path can show the same race but less frequently.
