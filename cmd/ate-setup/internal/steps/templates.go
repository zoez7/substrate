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

package steps

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/agent-substrate/substrate/cmd/ate-setup/internal/log"
	"github.com/agent-substrate/substrate/internal/atemanifest"
	"github.com/agent-substrate/substrate/internal/ateclient"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

// TemplateManifest is one substrate ActorTemplate manifest a demo installs.
type TemplateManifest struct {
	// Path is the protojson-shaped *.yaml.tmpl path, relative to the
	// repository root.
	Path string

	// Ref names the template the manifest creates. It is spelled out rather
	// than parsed from the manifest so that delete works without rendering.
	Ref TemplateRef

	// GoldenTimeout bounds the golden-snapshot wait. Zero means DemoTimeout;
	// micro-VM templates pass more because the guest cold boot is slower.
	GoldenTimeout time.Duration
}

// AteClient returns the control-plane client, dialing ate-api-server on first
// use. The connection is shared for the rest of the process.
func (e *Env) AteClient(ctx context.Context) (*ateclient.Client, error) {
	if e.ate == nil {
		client, err := ateclient.NewClient(ctx, e.Cfg.Kubeconfig, e.Cfg.Context, "", "", false)
		if err != nil {
			return nil, fmt.Errorf("while connecting to ate-api-server: %w", err)
		}
		e.ate = client
	}
	return e.ate, nil
}

// EnsureAtespace creates an atespace, treating one that already exists as
// success: an atespace is only a name, so there is no spec to have drifted.
func (e *Env) EnsureAtespace(ctx context.Context, name string) error {
	client, err := e.AteClient(ctx)
	if err != nil {
		return err
	}
	_, err = client.CreateAtespace(ctx, &ateapipb.CreateAtespaceRequest{
		Atespace: &ateapipb.Atespace{Metadata: &ateapipb.ResourceMetadata{Name: name}},
	})
	if err != nil && status.Code(err) != codes.AlreadyExists {
		return fmt.Errorf("while creating atespace %s: %w", name, err)
	}
	return nil
}

// CreateActorTemplate resolves the ko:// images in a rendered manifest and
// creates the substrate ActorTemplate the manifest holds, the
// `run_ko resolve | kubectl ate create actor-template -f -` of the shell
// installer. Templates are immutable, so one that already exists is kept;
// delete the demo and redeploy to replace it.
func (e *Env) CreateActorTemplate(ctx context.Context, rendered []byte) error {
	resolved, err := e.KoResolveBytes(ctx, rendered)
	if err != nil {
		return err
	}
	template, err := atemanifest.ParseActorTemplate(resolved)
	if err != nil {
		return err
	}
	ref := template.GetMetadata()
	client, err := e.AteClient(ctx)
	if err != nil {
		return err
	}
	if _, err := client.CreateActorTemplate(ctx, &ateapipb.CreateActorTemplateRequest{ActorTemplate: template}); err != nil {
		if status.Code(err) == codes.AlreadyExists {
			log.Stepf("ActorTemplate %s/%s already exists; keeping it (delete the demo to replace it)", ref.GetAtespace(), ref.GetName())
			return nil
		}
		return fmt.Errorf("while creating ActorTemplate %s/%s: %w", ref.GetAtespace(), ref.GetName(), err)
	}
	return nil
}

// WaitActorTemplateGolden blocks until the template's golden snapshot is
// built, the wait_actortemplate_ready of the shell installer. A build error
// reported in the status fails immediately rather than waiting out the
// timeout.
func (e *Env) WaitActorTemplateGolden(ctx context.Context, ref TemplateRef, timeout time.Duration) error {
	client, err := e.AteClient(ctx)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var lastStatus *ateapipb.GoldenSnapshotStatus
	var lastErr error
	for {
		at, err := client.GetActorTemplate(ctx, &ateapipb.GetActorTemplateRequest{
			ActorTemplate: &ateapipb.ObjectRef{Atespace: ref.Atespace, Name: ref.Name},
		})
		lastErr = err
		if err == nil {
			lastStatus = at.GetStatus().GetGoldenSnapshotStatus()
			if lastStatus.GetGoldenSnapshot() != nil {
				return nil
			}
			if msg := lastStatus.GetErrorMessage(); msg != "" {
				return fmt.Errorf("ActorTemplate %s/%s failed to build its golden snapshot: %s", ref.Atespace, ref.Name, msg)
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out after %v waiting for ActorTemplate %s/%s golden snapshot (last status %v, err %v)",
				timeout, ref.Atespace, ref.Name, lastStatus, lastErr)
		case <-time.After(5 * time.Second):
		}
	}
}

// DeleteActorTemplates removes the given templates and then their atespaces.
// As with DeleteDemoActors, a cluster without a reachable ate-api-server is
// not an error: DeleteAll runs this for demos that were never installed.
func (e *Env) DeleteActorTemplates(ctx context.Context, refs ...TemplateRef) error {
	if len(refs) == 0 {
		return nil
	}

	present, err := e.Kube.DeploymentExists(ctx, NamespaceAteSystem, "ate-api-server")
	if err != nil {
		return err
	}
	if !present {
		log.Step("ate-api-server not found; skipping template cleanup")
		return nil
	}

	client, err := e.AteClient(ctx)
	if err != nil {
		log.Warnf("could not connect to ate-api-server; skipping template cleanup: %v", err)
		return nil
	}

	// All templates before any atespace: an atespace only deletes once empty,
	// and demos like multi-template keep several templates in one atespace.
	for _, ref := range refs {
		log.Stepf("Deleting ActorTemplate %s/%s", ref.Atespace, ref.Name)
		if _, err := client.DeleteActorTemplate(ctx, &ateapipb.DeleteActorTemplateRequest{
			ActorTemplate: &ateapipb.ObjectRef{Atespace: ref.Atespace, Name: ref.Name},
		}); err != nil && status.Code(err) != codes.NotFound {
			log.Warnf("while deleting ActorTemplate %s/%s: %v", ref.Atespace, ref.Name, err)
		}
	}
	seen := map[string]bool{}
	for _, ref := range refs {
		if seen[ref.Atespace] {
			continue
		}
		seen[ref.Atespace] = true
		if _, err := client.DeleteAtespace(ctx, &ateapipb.DeleteAtespaceRequest{
			Atespace: &ateapipb.ObjectRef{Name: ref.Atespace},
		}); err != nil && status.Code(err) != codes.NotFound {
			log.Warnf("while deleting atespace %s: %v", ref.Atespace, err)
		}
	}
	return nil
}
