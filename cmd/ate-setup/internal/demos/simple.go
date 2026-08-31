// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package demos

import (
	"context"

	"github.com/spf13/pflag"

	"github.com/agent-substrate/substrate/cmd/ate-setup/internal/kube"
	"github.com/agent-substrate/substrate/cmd/ate-setup/internal/log"
	"github.com/agent-substrate/substrate/cmd/ate-setup/internal/render"
	"github.com/agent-substrate/substrate/cmd/ate-setup/internal/steps"
)

// bucketNamePlaceholder is substituted into every demo template with the
// snapshot bucket for this environment.
const bucketNamePlaceholder = "BUCKET_NAME"

// ExternalVolumePlaceholders are the optional external-volume hooks in the
// counter and autoscaled-workerpool templates. Every path except
// `deploy demo counter --with-external-volume` drops them, which removes the
// lines entirely.
var ExternalVolumePlaceholders = []string{
	"VALIDATE_EXISTING_FILE_PATH_ARG",
	"EXTERNAL_VOLUME_MOUNTS",
	"EXTERNAL_VOLUMES",
}

// Render expands a demo template with the configured bucket name.
func Render(e *steps.Env, relPath string, extraValues map[string]string, drop []string) ([]byte, error) {
	values := map[string]string{bucketNamePlaceholder: e.Cfg.BucketName}
	for k, v := range extraValues {
		values[k] = v
	}
	return render.Template(e.Cfg.Path(relPath), values, drop)
}

// RenderTemplateManifest expands a substrate ActorTemplate manifest. The
// external-volume placeholders that extraValues does not supply are dropped,
// so the same manifest serves both the plain and the external-volume deploys.
func RenderTemplateManifest(e *steps.Env, relPath string, extraValues map[string]string) ([]byte, error) {
	drop := make([]string, 0, len(ExternalVolumePlaceholders))
	for _, p := range ExternalVolumePlaceholders {
		if _, ok := extraValues[p]; !ok {
			drop = append(drop, p)
		}
	}
	return Render(e, relPath, extraValues, drop)
}

// Simple covers the demos that are one workload manifest plus a fixed set of
// ActorTemplates: render, ko apply, create the templates, wait; and on delete,
// remove the actors, the templates, then the rendered manifest.
//
// Demos that need more embed it and override a method, calling back into
// DeployWorkload and WaitReady to keep the shared ordering.
type Simple struct {
	// DemoName is the registry name, e.g. "demo-counter". It cannot be called
	// Name: that is the accessor the Demo interface requires.
	DemoName string
	// Short is the one-line summary, in cobra's sense of the word.
	Short string

	// Template is the *.yaml.tmpl path of the Kubernetes manifest, relative to
	// the repository root: the demo's namespace and WorkerPool.
	Template string

	// TemplateManifests are the demo's substrate ActorTemplate manifests,
	// created through the control-plane API after the workload manifest is
	// applied.
	TemplateManifests []steps.TemplateManifest

	// TemplateExtraValues optionally supplies extra placeholder values for the
	// TemplateManifests render, e.g. the counter demo's external-volume
	// stanzas. Placeholders it does not supply are dropped.
	TemplateExtraValues func(e *steps.Env) map[string]string

	// Deployments are the Deployments to wait for at deploy time, in order.
	// The WorkerPool controller names each Deployment after its WorkerPool.
	Deployments []steps.TemplateRef

	// SkipReadinessWait deploys without blocking on the ActorTemplates. The
	// sandbox demo sets this: it has no long-lived workload, and its template
	// is exercised on demand by the client rather than at install time. The
	// templates themselves are still created.
	SkipReadinessWait bool
}

func (d *Simple) Name() string        { return d.DemoName }
func (d *Simple) Description() string { return d.Short }

// Flags registers nothing: most demos take no options.
func (d *Simple) Flags(*pflag.FlagSet) {}

// TemplatePath exposes the demo's workload manifest through the Demo
// interface, so tests can check that every template renders cleanly.
func (d *Simple) TemplatePath() string { return d.Template }

// SubstrateTemplateManifests exposes the demo's substrate ActorTemplate
// manifests, so tests can check that every one renders and parses cleanly.
func (d *Simple) SubstrateTemplateManifests() []steps.TemplateManifest {
	return d.TemplateManifests
}

func (d *Simple) Deploy(ctx context.Context, e *steps.Env) error {
	log.Step(d.DemoName + "_deploy")
	if err := e.EnsureCRDs(ctx); err != nil {
		return err
	}
	if err := d.DeployWorkload(ctx, e); err != nil {
		return err
	}
	return d.WaitReady(ctx, e)
}

// DeployWorkload renders the demo's workload manifest and applies it through
// ko, without waiting. Demos that install add-ons alongside the workload call
// this directly so they can order the add-ons against it.
func (d *Simple) DeployWorkload(ctx context.Context, e *steps.Env) error {
	manifest, err := Render(e, d.Template, nil, ExternalVolumePlaceholders)
	if err != nil {
		return err
	}
	return e.KoApplyBytes(ctx, manifest)
}

// WaitReady blocks until the demo's workloads and templates are usable: the
// WorkerPool Deployments are rolled out, the substrate ActorTemplates are
// created, and every template has its golden snapshot.
//
// On a cold cluster the first ActorTemplate pays one-time costs: downloading
// the gVisor runsc binary, the first gVisor pod start, and image pulls.
// Blocking here means callers -- notably the e2e suite, which creates its own
// ActorTemplate with a tight readiness deadline -- run against an already-warm
// node instead of racing that cold-start work.
func (d *Simple) WaitReady(ctx context.Context, e *steps.Env) error {
	if !d.SkipReadinessWait && len(d.Deployments) > 0 {
		log.Stepf("Waiting for %s to be ready...", d.DemoName)
		for _, ref := range d.Deployments {
			if err := e.Kube.RolloutStatus(ctx, kube.KindDeployment, ref.Atespace, ref.Name, steps.DemoTimeout); err != nil {
				return err
			}
		}
	}
	// Created even under SkipReadinessWait: skipping the wait must not skip
	// the template.
	for _, m := range d.TemplateManifests {
		if err := e.EnsureAtespace(ctx, m.Ref.Atespace); err != nil {
			return err
		}
		var extra map[string]string
		if d.TemplateExtraValues != nil {
			extra = d.TemplateExtraValues(e)
		}
		rendered, err := RenderTemplateManifest(e, m.Path, extra)
		if err != nil {
			return err
		}
		if err := e.CreateActorTemplate(ctx, rendered); err != nil {
			return err
		}
	}
	if d.SkipReadinessWait {
		return nil
	}
	for _, m := range d.TemplateManifests {
		timeout := m.GoldenTimeout
		if timeout == 0 {
			timeout = steps.DemoTimeout
		}
		if err := e.WaitActorTemplateGolden(ctx, m.Ref, timeout); err != nil {
			return err
		}
	}
	return nil
}

// templateRefs collects the demo's templates for actor and template cleanup
// at delete time.
func (d *Simple) templateRefs() []steps.TemplateRef {
	refs := make([]steps.TemplateRef, 0, len(d.TemplateManifests))
	for _, m := range d.TemplateManifests {
		refs = append(refs, m.Ref)
	}
	return refs
}

func (d *Simple) Delete(ctx context.Context, e *steps.Env) error {
	log.Step(d.DemoName + "_delete")
	if err := e.DeleteDemoActors(ctx, d.templateRefs()...); err != nil {
		return err
	}
	if err := e.DeleteActorTemplates(ctx, d.templateRefs()...); err != nil {
		return err
	}
	manifest, err := Render(e, d.Template, nil, ExternalVolumePlaceholders)
	if err != nil {
		return err
	}
	return e.Kube.DeleteBytes(ctx, manifest)
}
