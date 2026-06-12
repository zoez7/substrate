# Reproducing the cold-start `connection termination` 503

This reproduces the intermittent error seen when curling an actor through the router:

```
upstream connect error or disconnect/reset before headers. reset reason: connection termination
```

**Root cause (confirmed):** a cold-start readiness race on the **restore** path. The resume
workflow marks the actor `STATUS_RUNNING` the instant `runsc restore` detaches — with no `:80`
readiness probe (`cmd/ateapi/internal/controlapi/workflow_resume.go:225,285`) — and the Envoy
route has no `retry_policy` (`cmd/atenet/internal/app/router/xds.go:270-297`). If Envoy connects
to `workerIP:80` in the few-millisecond window before the freshly-restored gVisor sandbox is
accepting on `:80`, Envoy returns `503` with flag `UC` (upstream connection termination), and
that single transient reset is surfaced straight to the client.

The error is **data-plane only**: ext_proc/`ResumeActor` succeeds and routes correctly. Control
plane failures (no free workers, actor not found) produce *different*, clean bodies (e.g.
`503 actor "f1" unavailable: no free workers available`), not this Envoy reset.

---

## 0. Prerequisites

```bash
cd /path/to/substrate
source .ate-dev-env.sh        # PROJECT_ID, BUCKET_NAME, KO_DOCKER_REPO, cluster context
go install ./cmd/kubectl-ate  # CLI plugin, if not already installed
```

The multi-template demo must be installed and **healthy** (see §1 to verify / repair). It uses:

| Resource | Namespace |
| --- | --- |
| `WorkerPool/shared-pool` (replicas: 3) | `ate-demo-multi-template-pool` |
| `ActorTemplate/counter` | `ate-demo-multi-template-counter` |
| `ActorTemplate/fspersist` | `ate-demo-multi-template-fspersist` |
| actors `c1` (counter), `f1` (fspersist) | created via CLI |

---

## 1. Establish a healthy baseline

```bash
# Pool pods must be Running (not CrashLoopBackOff) and workers must be FREE.
kubectl get pods -n ate-demo-multi-template-pool -o wide
kubectl ate get workers
kubectl get actortemplate -A          # both should exist; check Ready below
kubectl get actortemplate counter   -n ate-demo-multi-template-counter   -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}{"\n"}'
kubectl get actortemplate fspersist -n ate-demo-multi-template-fspersist -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}{"\n"}'
```

If the pool pods are in `CrashLoopBackOff` with
`unknown shorthand flag: 'p' in -pod-uid=...`, the cluster's `ate-controller` is stale (emits
single-dash `-pod-uid` while the `ateom-gvisor` binary now requires `--pod-uid`). Repair by
redeploying the control plane from HEAD, then the pool reconciles automatically:

```bash
./hack/install-ate.sh --deploy-ate-system
# wait for pool pods to become Running, then re-check `kubectl ate get workers`
```

Full demo reset (deletes leaked actor records + snapshots, then redeploys):

```bash
for id in $(kubectl ate get actors -o name 2>/dev/null); do kubectl ate delete actor "$id"; done
./hack/install-ate.sh --delete-demo-multi-template
./hack/install-ate.sh --deploy-demo-multi-template
kubectl ate create actor c1 --template ate-demo-multi-template-counter/counter
kubectl ate create actor f1 --template ate-demo-multi-template-fspersist/fspersist
```

Port-forward the router (leave running in its own terminal):

```bash
kubectl port-forward -n ate-system svc/atenet-router 8000:80
```

Sanity check — both should return `200`:

```bash
curl -s -H "Host: c1.actors.resources.substrate.ate.dev" http://localhost:8000; echo
curl -s -H "Host: f1.actors.resources.substrate.ate.dev" http://localhost:8000; echo
```

---

## 2. Reproduce the error

The race only fires on a **cold start**, so force one each iteration by suspending the actor and
immediately curling it. `fspersist` (`f1`) takes the restore path and reproduces most reliably.

```bash
for round in $(seq 1 20); do
  kubectl ate suspend actor f1 >/dev/null 2>&1
  code=$(curl -s -o /tmp/body -w "%{http_code}" --max-time 12 \
    -H "Host: f1.actors.resources.substrate.ate.dev" http://localhost:8000)
  echo "round $round: HTTP $code | $(head -c 90 /tmp/body | tr '\n' ' ')"
done
```

Expected: most rounds return `200`, and roughly 1 in 8–10 returns:

```
round N: HTTP 503 | upstream connect error or disconnect/reset before headers. reset reason: connection termin
```

Tip: increase the iteration count, or drop any sleep, to raise the hit rate. To see the failing
request id (used for log correlation in §3), capture it explicitly:

```bash
kubectl ate suspend actor f1 >/dev/null 2>&1
RID=$(uuidgen)
echo "request-id: $RID"
curl -s -H "Host: f1.actors.resources.substrate.ate.dev" \
     -H "x-request-id: $RID" http://localhost:8000; echo
```

---

## 3. Observe each part of the system to confirm the behaviour

Run these right after a failing round. The goal is to show, for one failing request, that the
**control plane succeeded and routed**, but the **upstream connection to `workerIP:80` was
terminated** (flag `UC`) because the restore had only just completed.

### 3a. ext_proc routing decision (atenet-router) — control plane succeeded

```bash
kubectl logs -n ate-system deploy/atenet-router -c atenet-router --tail=400 \
  | grep -iE "f1|ResumeActor result|Route ok|Error during ext_proc|no free"
```

For the failing request you should see, with `err:null`:

```
"msg":"ResumeActor result", ... "status":STATUS_RUNNING ... "worker_ip":"10.68.1.53", "err":null
"msg":"Route ok","actorID":"f1","targetAddr":"10.68.1.53:80"
```

`Route ok` proves ext_proc handed Envoy a valid target — the failure is downstream, not a
control-plane error. (If instead you see `Error during ext_proc` / `no free workers`, that's a
*different* problem — capacity, not this race.)

### 3b. Envoy access log — the upstream reset (flag `UC`)

```bash
kubectl logs -n ate-system deploy/atenet-router -c envoy --tail=500 \
  | grep -iE "\" 503 |UC|UF|reset|connection termination"
```

The failing line shows response code `503` and flag **`UC`** (upstream connection termination),
with the worker IP as both the route target and host:

```
[..T20:38:18.610Z] "GET / HTTP/1.1" 503 UC 0 95 296 - "-" "curl/8.7.1" "<request-id>" "10.68.1.53:80" "10.68.1.53:80"
```

- `UC` = upstream sent RST/FIN before response headers (exactly the client-visible message).
- `UF` would mean connect failure (refused). Either way it's the actor not accepting on `:80`.
- Filter to one request: append `| grep <request-id>`.

### 3c. Worker `ateom` log — restore timing (the race window)

Map the worker IP from §3a to its pod, then read the `ateom` log on that pod:

```bash
WORKER_IP=10.68.1.53   # from §3a "worker_ip"
POD=$(kubectl get pods -n ate-demo-multi-template-pool -o wide \
        --no-headers | awk -v ip="$WORKER_IP" '$6==ip{print $1}')
echo "worker pod: $POD"

kubectl logs -n ate-demo-multi-template-pool "$POD" --tail=300 \
  | grep -iE "RestoreWorkload|Actor restored|interior netns|runsc restore|Handled request|error"
```

What confirms the race: the `RestoreWorkload` RPC returns `"Actor restored"` (and the control
plane stamps `STATUS_RUNNING`) at almost the same millisecond Envoy logs the `UC` reset in §3b —
i.e. Envoy connected to `:80` in the window between "restore detached" and "sandbox actually
accepting". On a *successful* round you'll instead see the full
`Restoring eth0 routes/addresses in interior netns` → `runsc create`/`restore` →
`Actor restored` → `Handled request` sequence complete before the request is served.

### 3d. (Optional) Bypass Envoy — hit the actor's `:80` directly

To show the actor *itself* is the bottleneck (reachable a moment later), connect to `workerIP:80`
from inside the cluster network immediately after a resume:

```bash
kubectl run dbg --rm -it --image=curlimages/curl --restart=Never -- \
  curl -sv --max-time 3 http://10.68.1.53:80/
```

Reachable directly but flaky through Envoy → timing/race. Unreachable right after resume but fine
a second later → confirms the listener/networking warm-up gap.

### 3e. (Optional) Watch actor state transitions

```bash
watch -n 0.5 'kubectl ate get actors; echo; kubectl ate get workers'
```

`f1` flips `STATUS_SUSPENDED` → `STATUS_RESUMING` → `STATUS_RUNNING` on each round; the 503 occurs
on the first request that races the `RESUMING → RUNNING` restore.

---

## 4. Cleanup

```bash
# Stop the port-forward (Ctrl-C in its terminal), then optionally:
kubectl ate delete actor c1
kubectl ate delete actor f1
./hack/install-ate.sh --delete-demo-multi-template
```

---

## What this proves

| Claim | Evidence |
| --- | --- |
| Not a control-plane error | §3a `Route ok`, `err:null`, `STATUS_RUNNING` |
| Not a stale worker IP | same worker IP serves other rounds with `200` |
| Upstream terminated the connection | §3b Envoy flag `UC`, code `503` |
| Race is on the restore path | §3c `RestoreWorkload`/`Actor restored` timestamp ≈ Envoy reset |
| Surfaces to client | no `:80` readiness gate (`workflow_resume.go:225,285`); no Envoy retry (`xds.go:270-297`) |
