# Plan: replace substrate template block placeholders with post-decode proto mutation

Status: planned, not implemented. Working doc — do not commit.

## Goal

The substrate ActorTemplate fixture manifests (`internal/e2e/fixtures/**/*-template*.yaml.tmpl`)
currently carry three whole-line block placeholders — `${TEMPLATE_SANDBOX_CONFIG}`,
`${TEMPLATE_RESOURCES}`, `${TEMPLATE_TRUST_BUNDLE}` — filled with `\n`-escaped,
indentation-sensitive YAML fragments by `substrateTemplateSubstitutions`
(internal/e2e/fixture.go). Since `decodeSubstrateTemplates` already yields
`*ateapipb.ActorTemplate` protos, apply the sandbox-class / resources / trust-bundle
variants by mutating the decoded proto instead. A misspelled field becomes a compile
error; the indentation magic disappears.

Keep the inline placeholders (`${BUCKET_NAME}`, `${FIXTURE_SUFFIX}`) — they appear
inside string values (names, `gs://` paths) and must stay textual. The pool half
(`fixtureSubstitutions` blocks, `${WORKERPOOL_RUNTIME}`) is untouched: its target is
`ko apply`, not the ate API.

## 1. internal/e2e/fixture.go

Add:

```go
// applyTemplateVariants mutates a decoded fixture template for the sandbox
// class under test. Doing this on the decoded proto rather than as text
// fragments makes a misspelled field a compile error.
func applyTemplateVariants(t *testing.T, tmpl *ateapipb.ActorTemplate, trustBundle bool)
```

Behavior (carry over the existing comments from `substrateTemplateSubstitutions`):

- **Sandbox config — always set by Go**, never in the manifest (per-class by nature;
  writing a gVisor default into 6 docs recreates the drift problem):
  - gVisor: `SANDBOX_CLASS_GVISOR` + config name `gvisor-default` (the cluster-wide
    default manifests/ate-install ships; config_name is required so it is named
    explicitly).
  - micro-VM (`IsMicroVM()`): `SANDBOX_CLASS_MICROVM` + config name `microvm`
    (deliberately not the class default, so a missing/stale install fails loudly).
- **micro-VM default resources** only when the template declares none:
  `if IsMicroVM() && len(tmpl.GetResources().GetLimits()) == 0` → cpu "1" /
  memory 512Mi. This turns the current placement convention ("probe-sized-template
  carries no ${TEMPLATE_RESOURCES} placeholder") into self-describing code.
- **trustBundle**: when true, append a `TrustBundleDataSource{Name: ..., Path:
  "trust-bundle.pem"}` to the first volume with `systemInfo` set; `t.Fatalf` if the
  template has no systemInfo volume. Derive the name from the same source as
  `EgressTrustBundleObjectName` (internal/e2e/trustbundle.go) instead of a second
  string literal — probe_test.go asserts they agree.

Delete `substrateTemplateSubstitutions` entirely. In `DeploySubstrateFixture`,
render both halves from one `fixtureSubstitutions(bucket, name)` call (extra inline
`${ATEOM_IMAGE}` and unmatched block keys are no-ops in the template docs), then
after `decodeSubstrateTemplates`, call `applyTemplateVariants` on each doc before
`CreateActorTemplate`. `koResolve` runs on rendered text before decode — unchanged.

Update the `DeploySubstrateFixture` doc comment: class variants are applied in Go
post-decode.

Exact `ateapipb` field names for `SandboxConfig` / `Resources` / limits /
`TrustBundleDataSource` need checking against pkg/proto/ateapipb at implementation
time; no other unknowns.

## 2. Template manifests — delete placeholder lines

- `internal/e2e/fixtures/probe/probe-template.yaml.tmpl`: drop lines 59–61
  (`${TEMPLATE_TRUST_BUNDLE}`, `${TEMPLATE_RESOURCES}`, `${TEMPLATE_SANDBOX_CONFIG}`).
  The "Only for suites that pass WithTrustBundle" comment moves to Go or points at
  `applyTemplateVariants`.
- `internal/e2e/fixtures/probe/probe-sized-template.yaml.tmpl`: drop
  `${TEMPLATE_SANDBOX_CONFIG}` (line 50); rewrite the line-19 comment that mentions
  the missing `${TEMPLATE_RESOURCES}` placeholder — now it declares its own limits,
  so `applyTemplateVariants` leaves them alone.
- `internal/e2e/fixtures/capabilities/capabilities-templates.yaml.tmpl`: drop the
  two placeholder pairs (lines 39–40, 71–72 — both docs).
- `internal/e2e/fixtures/testserver/grpcecho-template.yaml.tmpl`: drop lines 48–49.
- `internal/e2e/fixtures/testserver/websocket-template.yaml.tmpl`: drop lines 34–35.

## 3. Tests

- `internal/e2e/sandbox_test.go` `renderTemplates`: render with the inline
  substitutions, decode, then `applyTemplateVariants(t, tmpl, false)` on each doc.
  The assertions in `TestRenderSubstrateFixtures_GVisor/_MicroVM` stay exactly as
  they are — they pin the final proto (sandboxClass, configName, memory limit,
  snapshot location), now covering render+mutation instead of render alone.
- `internal/e2e/probe_test.go` `TestProbeTemplate_TrustBundle`: decode then mutate;
  drop the now-stale "strict decoding proves the fragment landed at the right depth"
  comment (there is no fragment anymore) — the test pins the opt-in on/off plus
  name/path.
- Strict decode keeps its remaining job: guarding the hand-written .tmpl content and
  inline placeholders, same contract as `kubectl ate create actor-template`.

## 4. Untouched

- Pool half: `fixtureSubstitutions` blocks, `renderPool`, `${WORKERPOOL_RUNTIME}`.
- All suite callers (`DeploySubstrateFixture` signature unchanged): probe.go,
  capabilities, sizing, networking suites.

## Follow-up (separate commit, out of scope)

Uncommitted `internal/atemanifest.ParseActorTemplate` duplicates the single-doc
logic of `decodeSubstrateTemplates`. Once that package lands, change
`decodeSubstrateTemplates` to split on `---` and call it per doc.

## Verification

`make test` (TestRenderSubstrateFixtures_* and TestProbeTemplate_TrustBundle cover
both lanes without a cluster), then `make verify`.
