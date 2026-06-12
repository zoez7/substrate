# ATE API Server Authentication

Status: **Implemented** (see Work items below; follow-ups remain open)

This document describes the authentication design for the ate-apiserver: how
incoming requests are classified into principals using mTLS client
certificates and OIDC-verified JWTs, including JWTs forwarded by the atenet
router on behalf of actors.

Scope: **authentication only**. This design establishes *who* the caller is
(a `PrincipalInfo` in the request context) and rejects invalid credentials.
Per-RPC authorization (which principal kinds may call which methods) is a
named follow-up; until then, handlers perform minimal inline checks.

## Background

The ate-apiserver receives requests from several distinct caller classes:

* **System services** (e.g. the atenet router) calling the Control API. These
  run inside the mesh and hold service-dns client certificates.
* **Actors**, whose requests are routed *through* the atenet router. The
  router terminates the actor's connection, establishes its own mTLS
  connection to the apiserver, and forwards the actor's session-id JWT.
  Actors never present their own SPIFFE certificate directly to the
  apiserver.
* **Clients** bootstrapping a session: callers presenting their own
  Kubernetes-issued JWT to `SessionIdentity.MintJWT` in exchange for a
  session-id JWT.
* **Workerpool workloads**, whose certificates are accepted at the TLS layer
  but are not classified into a principal.

Two authentication mechanisms therefore coexist, in two relationships:

1. **Disjoint principal classes** — each caller class authenticates with
   exactly one mechanism. System services use mTLS; clients use a
   self-asserted JWT.
2. **Layered** — a JWT carried *on top of* an mTLS connection. The router's
   certificate proves the channel; the forwarded session-id JWT proves the
   actor. Trust in the forwarded JWT is conditional on the mTLS peer being an
   allowlisted forwarder.

## Principal model

```go
// Kind is an open string label. Well-known values are defined as constants,
// but JWT issuer configuration may introduce new labels without code changes.
type Kind string

const (
    // KindUnauthenticated is the zero value: no credential was presented.
    KindUnauthenticated Kind = ""
    // KindSystem identifies a service-dns mTLS client certificate. This label
    // is fixed in code; certificate classification is not config-driven.
    KindSystem Kind = "system"
    // KindActorJWT and KindK8sJWT are conventional labels assigned to JWT
    // issuers via configuration (see "OIDC issuer configuration").
    KindActorJWT Kind = "actor-jwt"
    KindK8sJWT Kind = "k8s-jwt"
)

type PrincipalInfo struct {
    Kind Kind
    // ID is the principal's identity: the allowlisted DNS SAN for
    // certificate-authenticated principals, or the configured identity claim
    // (default "sub") for JWT-authenticated principals.
    ID string

    // Verified JWT claims, exposed through typed accessors rather than a raw
    // map (e.g. K8s() for Kubernetes bound claims, Session() for substrate
    // app/user/session claims). Empty for certificate-authenticated
    // principals.

    // Forwarder is set only for forwarded identities: it is the
    // authenticated System identity of the mTLS peer (the atenet router)
    // that asserted this principal. Nil for directly-authenticated
    // principals.
    Forwarder *PrincipalInfo
}
```

Design choices:

* `Kind` is an **open string label**, not a closed enum. Issuer configuration
  maps each trusted issuer to a label; new principal classes require no code
  change. Code-level constants keep call sites readable.
* The empty string is the unauthenticated zero value, so absent credentials
  stay the safe default and `FromContext` never returns nil.
* Claims are exposed through **typed accessors**, not `map[string]any`, so
  consumers like `MintJWT` read namespace/service-account claims
  type-safely.
* `Forwarder` preserves the full delegation chain for audit: the effective
  principal is the actor, the forwarder is the router's System identity.

## Authentication sources

### 1. mTLS client certificates → `system`

Unchanged from the current `authn` package, minus the actor path:

* The TLS handshake accepts the union of the service-dns, session-id, and
  workerpool CA pools (`ClientAuth: VerifyClientCertIfGiven`), so all
  certificate types reach the interceptor.
* A leaf that chains to the **service-dns CA** and presents a DNS SAN on the
  `--system-dns-names` allowlist is classified as `Kind: system` with the SAN
  as ID. The allowlist fails closed; there is no CommonName fallback.
* **The direct SPIFFE-cert actor path is removed.** All actor identities
  arrive as router-forwarded JWTs, so certificates chaining to the session-id
  CA are no longer classified into a principal. (Whether the session-id CA is
  still needed in the handshake `ClientCAs` at all should be verified during
  implementation; no actor certificate is expected to terminate TLS at the
  apiserver.)
* Workerpool certificates remain accepted at the TLS layer and unclassified.

### 2. Forwarded actor JWTs → `actor` (layered)

Forwarding is **exclusively a router capability**:

* The atenet router forwards the actor's session-id JWT in a **dedicated
  header** (`x-substrate-forwarded-jwt`), never in `authorization`. The
  dedicated header makes "forwarded" an explicit channel decision rather than
  an inference from token contents.
* The interceptor honors the forwarding header **only** when the mTLS peer's
  authenticated System identity is on the trusted-forwarder allowlist
  (`--trusted-jwt-forwarders`, a set of DNS SANs, expected to be a subset of
  `--system-dns-names`). The allowlist fails closed: empty means no forwarded
  JWT is ever honored.
* A forwarding header from a peer that is **not** a trusted forwarder is
  rejected (`Unauthenticated`) — a well-behaved client never sets that
  header, so its presence from a non-router is treated as a spoofing
  attempt, not silently ignored.
* The forwarded token is verified through the OIDC pipeline below. On
  success the effective principal is the actor (Kind/ID from the issuer
  config), with `Forwarder` set to the router's System principal.

### 3. Self-asserted JWTs → issuer-configured kind

A caller may present its own JWT as `authorization: Bearer <token>`:

* Honored **only when the peer presents no classifiable certificate**. If the
  mTLS peer classifies as `system`, any Bearer token is ignored outright —
  not even verified (so an invalid token from a System peer is not a
  rejection; rejection applies only to credentials that are actually
  evaluated).
* The primary self-asserted caller today is the `MintJWT` client presenting a
  Kubernetes-issued JWT. With this design, that verification moves out of the
  `MintJWT` handler and into the interceptor: the Kubernetes cluster issuer
  becomes an ordinary OIDC issuer config entry mapping to `Kind: k8s-jwt`, and
  `MintJWT` reads the principal from context, keeping only an inline
  `Kind == k8s-jwt` check.

### Precedence

Evaluated top to bottom; the first matching rule wins:

1. **Forwarding header present**
   * peer ∈ trusted forwarders → OIDC-verify the forwarded token → `actor`
     with `Forwarder` set; invalid token → **reject**.
   * peer ∉ trusted forwarders → **reject**.
2. **Certificate classifies to `system`** → that is the principal; any
   `authorization` Bearer token is ignored.
3. **No classified certificate, Bearer token present** → OIDC-verify → Kind
   from issuer config; invalid → **reject**.
4. **Nothing presented** → `Kind: ""` (unauthenticated), passed through.

### Rejection semantics

* **Absent credentials pass through** as unauthenticated. Some methods are
  intentionally reachable without credentials (health, reflection); handlers
  and the future authz layer decide.
* **Present-but-invalid credentials are rejected** at the interceptor with
  `codes.Unauthenticated`: bad signature, expired, untrusted issuer, wrong
  audience, or a forwarding header from a non-forwarder. A credential that
  fails verification has no legitimate use; passing it through as anonymous
  would hide tampering.
* **Key-fetch failure is not invalidity.** If issuer discovery or JWKS
  retrieval fails, the request fails with `codes.Unavailable` (retryable) —
  the token may be perfectly valid. Only verification failures map to
  `Unauthenticated`.

## OIDC issuer configuration

All JWT verification — forwarded and self-asserted — goes through a single
multi-issuer OIDC pipeline configured by `--oidc-configs`, a mounted JSON
file (consistent with the other trust inputs, which are all mounted files or
projected volumes):

```json
{
  "issuers": [
    {
      "issuer": "https://<session-id-broker>",
      "kind": "actor-jwt",
      "idClaim": "sub",
      "audiences": ["<ate-apiserver audience>"]
    },
    {
      "issuer": "https://<kubernetes cluster issuer>",
      "kind": "k8s-jwt",
      "audiences": ["<ate-apiserver audience>"]
    }
  ]
}
```

Per entry:

* `issuer` — the expected `iss` claim. Keys are discovered via the issuer's
  `/.well-known/openid-configuration` and its `jwks_uri`.
* `kind` — the principal label assigned to tokens from this issuer
  (declarative issuer → Kind mapping; open label).
* `idClaim` — which verified claim becomes `PrincipalInfo.ID` (default
  `sub`).
* `audiences` — allowlist; the token's `aud` must intersect it. Tokens
  presented to the apiserver are expected to carry the apiserver's own
  identity as audience (an actor mints a token *for* the apiserver before
  calling it). Per-request target-audience matching on the actor→actor data
  plane is enforced at the destination actor, not here.

A token whose issuer is not configured is invalid (fails closed).

### The session-id broker becomes an OIDC issuer

Today the broker signs session-id JWTs from a mounted signing pool
(`--session-id-jwt-pool`) and there is no verification path. Under this
design the broker **serves OIDC discovery** (`/.well-known/openid-configuration`
and a JWKS endpoint publishing the pool's public keys), so the apiserver
verifies session-id JWTs through the same OIDC pipeline as every other
issuer — no file-based special case, and key rotation comes for free. The
mounted signing pool remains broker-side, signing-only.

This also forces resolving the existing TODO: the broker's issuer URL must
be globally unique and routable for discovery.

### Verifier implementation

v1 **reuses the existing `internal/k8sjwt` discovery pipeline as-is** —
discovery document → JWKS → signature verification → time/audience checks —
generalized in two ways:

* **Multi-issuer**: expected issuer/audience come from the matched
  `--oidc-configs` entry rather than fixed flags.
* **Raw verified claims**: the verification core returns generic verified
  claims; Kubernetes-specific claim parsing becomes one per-issuer mapping
  (the broker issuer maps to substrate app/user/session claims instead).

Known v1 limitation, accepted deliberately: **no key caching**. Every
JWT-authenticated RPC performs an outbound discovery + JWKS fetch, adding
latency and a per-request dependency on issuer endpoints. This is tolerable
for the current call volume but is **follow-up #1** (see below), with the
intended design already agreed: startup prefetch (best-effort), background
refresh on a TTL, bounded on-demand refetch on unknown `kid`, and
serve-last-known-good on transient fetch failure (short-lived tokens and
slow key rotation make brief staleness low-risk, while rejecting all traffic
on a discovery blip is the worse failure).

The `--oidc-configs` file is read at startup; live reload of the issuer set
is deferred along with caching.

## Interceptor

The `authn` interceptor runs first in the chain so the principal is in
context for logging and handlers. It covers **both unary and streaming**
RPCs (`UnaryServerInterceptor` and `StreamServerInterceptor` sharing one
`authenticate()`); the current implementation is unary-only.

## Configuration summary

| Flag | Purpose |
| :--- | :--- |
| `--system-dns-names` | DNS SAN allowlist for `system` certificate principals (existing). |
| `--service-dns-ca-certs` | CA bundle that signs system certificates (existing). |
| `--trusted-jwt-forwarders` | DNS SANs of System identities allowed to assert forwarded actor JWTs (new; subset of `--system-dns-names`; fails closed). |
| `--oidc-configs` | Path to the mounted OIDC issuer configuration file (new). |
| `--actor-trust-domains` | Removed along with the direct SPIFFE-cert actor path. |
| `--client-jwt-issuer` / `--client-jwt-audience` | Superseded by an `--oidc-configs` entry once `MintJWT` reads the principal from context. |

Forwarding header: `x-substrate-forwarded-jwt`, set only by the atenet
router.

## Work items

1. ✅ **Generalize the JWT verifier** (`internal/oidcjwt`): multi-issuer OIDC
   verifier returning raw verified claims with typed accessors; the
   `--oidc-configs` loader; EC JWK parsing and a context-aware HTTP client
   with a custom CA bundle (both gaps in the old `k8sjwt` pipeline).
2. ✅ **Broker as OIDC issuer** (`cmd/ateapi/internal/oidcprovider`): serves
   `/.well-known/openid-configuration` and `/openid/v1/jwks` on a dedicated
   HTTPS listener (`--oidc-issuer-listen-addr`, container port 8443) behind
   the new `broker.ate-system.svc` Service; the issuer URL is configured via
   `--session-jwt-issuer` (the hardcoded placeholder is gone).
3. ✅ **Rework the `authn` interceptor**: precedence ladder; SPIFFE-cert
   actor path removed; forwarding-header + trusted-forwarder gate; rejects
   present-but-invalid; unary + streaming interceptors; `PrincipalInfo` with
   open-label Kind, `Token` claims accessors, and `Forwarder`. The forwarded
   header key lives in `internal/atemetadata`.
4. ✅ **Router forwarding**: atenet dials the apiserver with mTLS
   (`--ateapi-client-cred-bundle`, `--ateapi-server-ca`) and forwards the
   calling actor's Bearer token as `x-substrate-forwarded-jwt` on
   `ResumeActor`; credential-bearing headers are redacted from router logs.
5. ✅ **Migrate `MintJWT`**: reads the principal from context with an inline
   `Kind == k8s-jwt` check; the inline `k8sjwt.Verify` and the whole
   `internal/k8sjwt` package are gone.
6. ✅ **Manifests + install**: `hack/install-ate.sh` renders the
   `ate-api-server-oidc-configs` ConfigMap (cluster issuer → `client`,
   broker → `actor`); the apiserver mounts it and sets
   `--trusted-jwt-forwarders=atenet-router.ate-system.svc`;
   `--actor-trust-domains` and `--client-jwt-issuer`/`--client-jwt-audience`
   are removed.

## Follow-ups (explicitly out of scope)

1. **JWKS caching and live config reload** — background refresh,
   serve-stale-on-error, unknown-`kid` refetch; hot-reload of the issuer
   set and CA bundles (shares the existing rotation-reload TODO).
2. **Declarative authorization** — a method → allowed-Kinds policy enforced
   in an interceptor after authn, replacing inline handler checks. Needs its
   own design pass (policy shape, default-deny vs default-allow,
   per-method vs per-service).
