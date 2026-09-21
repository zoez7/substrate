# Actor Object Model × Compatibility Model

This folder is one review set: two proposals that land independently, a
companion design, and (below) the shared contracts they implement against.
Reading order:

1. `actor-instance-model.md` — Proposal A: self-contained Actors, optional
   decoupled ActorTemplates, managed groups, RBAC tiers.
2. `multi-arch-golden-snapshots.md` — Proposal B: worker platform reporting,
   CompatibilityKeys, golden matrix, scheduling eligibility, SandboxConfig
   upgrades.
3. `sandboxconfig-lifecycle.md` — companion to B: mutable SandboxConfig
   snapshotted into immutable digest-named SandboxConfigVersions, cluster
   defaults, disable/retention levers. Owns the `sandbox_config_digest`
   identity Contract 1 keys on and the record-side resolution Contract 3
   assumes.

The rest of this page is the shared contracts. Each contract states the
final shape, who owns which half, and the degenerate form the first-to-land
proposal may ship. Reviewers of any one document need only this page, not
the others in full.

## Contract 1: compatibility key

Final shape: a golden snapshot is identified by the tuple

```
(spec_hash, CompatibilityKey)
```

* A owns `spec_hash`: the canonical hash of the ActorSpec the snapshot was
  captured under, frozen into every snapshot as `ExternalSnapshot.spec_hash`.
  A defines the canonicalization (field ordering, defaulting) and its
  stability guarantees.
* B owns `CompatibilityKey`: `architecture`, `sandbox_class`,
  `sandbox_config_digest`, plus optional `platform_fingerprint` set only when
  the config declares no CPU feature baseline or the runtime cannot level
  (CHV, gVisor arm64).
* Golden eligibility at seed time is equality on the full tuple; neither half
  may be compared by name reference (template names and config names are
  pointers, not identities).
* Degenerate forms:
  * B lands first: `(template UID, CompatibilityKey)` — today's template-keyed
    golden, fanned out per CompatibilityKey.
  * A lands first: `(spec_hash, single implicit CompatibilityKey)` — one golden per
    spec_hash on an assumed-homogeneous fleet.
  * Either way the tuple's shape is fixed now; the later proposal fills its
    half without rekeying the other.

## Contract 2: fidelity ladder and trigger registry

Final shape: `minimumFidelity` is a per-call parameter on resume, with the
ladder

```
MEMORY > DATA > SPEC
```

* Semantics: the system restores at the highest fidelity currently possible
  that is at or above the floor; if none, the call fails. No waiting or
  parking — downgrade is immediate, and a `MEMORY` floor doubles as
  fail-fast (callers who care retry, which is the de facto park).
* A owns the mechanism: the parameter, the ladder, and the downgrade path
  (drop the memory layer → data-only restore → cold boot from spec).
  Defined once; never duplicated.
* Triggers are an open registry; each proposal appends conditions that force
  a downgrade below MEMORY:
  * spec_hash mismatch between the snapshot and the actor's current spec (A);
  * no compatible hardware for the snapshot's recorded platform (B);
  * retired runtime assets the snapshot's record pins (B).
* Adding a trigger must not change the ladder or the parameter's meaning.

## Contract 3: snapshot record schema

Final shape: the FULL snapshot's API record is the single source of truth for
what the snapshot is and what it needs; it replaces the storage-side
`manifest.json`.

* A contributes `spec_hash` (Contract 1).
* B contributes `CompatibilityKey` and the resolved runtime assets: a value copy
  of URLs + sha256 per asset, the pause image, and the applied CPU feature
  baseline. Never a config name reference — records freeze content,
  configs drift.
* DATA snapshot records hold a foreign key to their base golden/FULL record;
  restore resolves assets through that chain (DATA → base → assets).
* Minimal self-description (SandboxConfigVersion digest / runtime version) stays inside the
  machine state itself, so atelet can cross-check the request against the
  artifact before restoring. This is the only restore-side guard for CHV,
  which validates nothing on its own; gVisor's runsc version stamp is an
  independent backstop only for that class.
* Write ordering: blobs upload first; the record commit is what makes the
  snapshot exist. A record without blobs and blobs without a record are both
  orphans, and each direction needs a cleanup path.

## Non-contracts

* Scheduling constraint composition (A's selectors AND B's platform
  predicate) needs no coordination: constraints compose by conjunction.
* Golden build policy (eager/lazy, cost controls) is B-internal; group
  rolling-update knobs are A-internal.
