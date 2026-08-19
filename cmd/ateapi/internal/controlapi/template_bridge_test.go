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

package controlapi

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/agent-substrate/substrate/cmd/ateapi/internal/store"
	"github.com/agent-substrate/substrate/internal/resources"
	atev1alpha1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

var quantityComparer = cmp.Comparer(func(a, b resource.Quantity) bool { return a.Cmp(b) == 0 })

func TestActorTemplateFromProto(t *testing.T) {
	in := &ateapipb.ActorTemplate{
		Metadata: &ateapipb.ResourceMetadata{Atespace: "ns1", Name: "tmpl-a", Uid: "uid-1"},
		WorkerSelector: &ateapipb.Selector{
			MatchLabels: map[string]string{"pool": "gp"},
		},
		Containers: []*ateapipb.Container{{
			Name:    "main",
			Image:   "example.com/app@sha256:abc",
			Command: []string{"/bin/app"},
			Args:    []string{"--serve"},
			Env:     []*ateapipb.EnvVar{{Name: "MODE", Value: "prod"}},
			Readyz: &ateapipb.ContainerReadyz{
				HttpGet:        &ateapipb.HTTPGetAction{Path: "/healthz", Port: 8080},
				TimeoutSeconds: 60,
			},
			VolumeMounts: []*ateapipb.VolumeMount{{Name: "data", MountPath: "/data"}},
		}},
		Volumes: []*ateapipb.Volume{
			{Name: "data", DurableDir: &ateapipb.DurableDirVolumeSource{}},
			{Name: "scratch", ExternalVolumeTemplate: &ateapipb.ExternalVolumeTemplate{Capacity: "10Gi", StorageClassName: "fast"}},
		},
		SnapshotsConfig: &ateapipb.SnapshotsConfig{
			StorageLocation: "gs://bucket/snapshots",
			OnPause:         ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL,
			OnCommit:        ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA,
			OnResume:        &ateapipb.OnResumeConfig{FromData: ateapipb.ResumeSource_RESUME_SOURCE_GOLDEN},
		},
		SandboxConfig: &ateapipb.SandboxConfig{SandboxClass: ateapipb.SandboxClass_SANDBOX_CLASS_MICROVM, ConfigName: "microvm-default"},
		Resources: &ateapipb.Resources{Limits: []*ateapipb.Limits{
			{Name: "memory", Quantity: "512Mi"},
			{Name: "cpu", Quantity: "500m"},
		}},
		Status: &ateapipb.ActorTemplateStatus{
			Phase:          ateapipb.ActorTemplatePhase_ACTOR_TEMPLATE_PHASE_READY,
			GoldenSnapshot: &ateapipb.ObjectRef{Atespace: "ate-golden", Name: "snap-1"},
		},
	}

	got, err := actorTemplateFromProto(in)
	if err != nil {
		t.Fatalf("actorTemplateFromProto() error: %v", err)
	}

	want := &atev1alpha1.ActorTemplate{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "tmpl-a", UID: types.UID("uid-1")},
		Spec: atev1alpha1.ActorTemplateSpec{
			Containers: []atev1alpha1.Container{{
				Name:    "main",
				Image:   "example.com/app@sha256:abc",
				Command: []string{"/bin/app"},
				Args:    []string{"--serve"},
				Env:     []atev1alpha1.EnvVar{{Name: "MODE", Value: "prod"}},
				Readyz: &atev1alpha1.ContainerReadyz{
					HTTPGet:        &atev1alpha1.HTTPGetAction{Path: "/healthz", Port: 8080},
					TimeoutSeconds: 60,
				},
				VolumeMounts: []atev1alpha1.VolumeMount{{Name: "data", MountPath: "/data"}},
			}},
			SnapshotsConfig: atev1alpha1.SnapshotsConfig{
				Location: "gs://bucket/snapshots",
				OnPause:  atev1alpha1.SnapshotScopeFull,
				OnCommit: atev1alpha1.SnapshotScopeData,
				OnResume: atev1alpha1.OnResumeConfig{FromData: atev1alpha1.ResumeSourceGolden},
			},
			SandboxClass:   atev1alpha1.SandboxClassMicroVM,
			WorkerSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"pool": "gp"}},
			Volumes: []atev1alpha1.Volume{
				{Name: "data", VolumeSource: atev1alpha1.VolumeSource{DurableDir: &atev1alpha1.DurableDirVolumeSource{}}},
				{Name: "scratch", VolumeSource: atev1alpha1.VolumeSource{ExternalVolumeTemplate: &atev1alpha1.ExternalVolumeTemplate{
					Capacity:         resource.MustParse("10Gi"),
					StorageClassName: "fast",
				}}},
			},
			Resources: &corev1.ResourceRequirements{Limits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("512Mi"),
				corev1.ResourceCPU:    resource.MustParse("500m"),
			}},
		},
		Status: atev1alpha1.ActorTemplateStatus{GoldenSnapshot: "snap-1"},
	}
	if diff := cmp.Diff(want, got, quantityComparer); diff != "" {
		t.Errorf("actorTemplateFromProto() mismatch (-want +got):\n%s", diff)
	}
}

func TestActorTemplateFromProto_Defaults(t *testing.T) {
	got, err := actorTemplateFromProto(validActorTemplate(func(tmpl *ateapipb.ActorTemplate) {
		tmpl.Containers[0].Readyz = &ateapipb.ContainerReadyz{
			HttpGet: &ateapipb.HTTPGetAction{Port: 8080},
		}
	}))
	if err != nil {
		t.Fatalf("actorTemplateFromProto() error: %v", err)
	}

	// Kubebuilder defaults the API server would apply to a stored CRD.
	if got, want := got.Spec.Containers[0].Readyz.HTTPGet.Path, "/readyz"; got != want {
		t.Errorf("Readyz.HTTPGet.Path = %q, want %q", got, want)
	}
	if got, want := got.Spec.Containers[0].Readyz.TimeoutSeconds, int32(30); got != want {
		t.Errorf("Readyz.TimeoutSeconds = %d, want %d", got, want)
	}
	if got, want := got.Spec.SnapshotsConfig.OnPause, atev1alpha1.SnapshotScopeFull; got != want {
		t.Errorf("SnapshotsConfig.OnPause = %q, want %q", got, want)
	}
	if got, want := got.Spec.SnapshotsConfig.OnCommit, atev1alpha1.SnapshotScopeFull; got != want {
		t.Errorf("SnapshotsConfig.OnCommit = %q, want %q", got, want)
	}
	if got, want := got.Spec.SnapshotsConfig.OnResume.FromData, atev1alpha1.ResumeSourceColdBoot; got != want {
		t.Errorf("SnapshotsConfig.OnResume.FromData = %q, want %q", got, want)
	}
	if got, want := got.Spec.SandboxClass, atev1alpha1.SandboxClassGvisor; got != want {
		t.Errorf("SandboxClass = %q, want %q", got, want)
	}
}

func TestActorTemplateFromProto_Errors(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*ateapipb.ActorTemplate)
		wantErr string
	}{{
		"readyz without http_get",
		func(tmpl *ateapipb.ActorTemplate) {
			tmpl.Containers[0].Readyz = &ateapipb.ContainerReadyz{TimeoutSeconds: 10}
		},
		"readyz requires http_get",
	}, {
		"volume with no source",
		func(tmpl *ateapipb.ActorTemplate) {
			tmpl.Volumes = []*ateapipb.Volume{{Name: "data"}}
		},
		"exactly one of durable_dir or external_volume_template",
	}, {
		"volume with both sources",
		func(tmpl *ateapipb.ActorTemplate) {
			tmpl.Volumes = []*ateapipb.Volume{{
				Name:                   "data",
				DurableDir:             &ateapipb.DurableDirVolumeSource{},
				ExternalVolumeTemplate: &ateapipb.ExternalVolumeTemplate{Capacity: "1Gi", StorageClassName: "fast"},
			}}
		},
		"exactly one of durable_dir or external_volume_template",
	}, {
		"bad external volume capacity",
		func(tmpl *ateapipb.ActorTemplate) {
			tmpl.Volumes = []*ateapipb.Volume{{
				Name:                   "data",
				ExternalVolumeTemplate: &ateapipb.ExternalVolumeTemplate{Capacity: "not-a-quantity", StorageClassName: "fast"},
			}}
		},
		"invalid capacity",
	}, {
		"missing snapshots_config",
		func(tmpl *ateapipb.ActorTemplate) {
			tmpl.SnapshotsConfig = nil
		},
		"snapshots_config is required",
	}, {
		"unspecified sandbox class",
		func(tmpl *ateapipb.ActorTemplate) {
			tmpl.SandboxConfig.SandboxClass = ateapipb.SandboxClass_SANDBOX_CLASS_UNSPECIFIED
		},
		"sandbox_config.sandbox_class",
	}, {
		"bad resource limit quantity",
		func(tmpl *ateapipb.ActorTemplate) {
			tmpl.Resources = &ateapipb.Resources{Limits: []*ateapipb.Limits{{Name: "memory", Quantity: "bogus"}}}
		},
		"invalid quantity",
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := actorTemplateFromProto(validActorTemplate(tc.mutate))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("actorTemplateFromProto() error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

// seedSubstrateTemplateActor stores a substrate template (with the given
// mutations) plus an actor referencing it by ObjectRef, and returns the
// workflow to drive them with. The lister is empty: a substrate-ref actor
// must never fall back to the CRD path.
func seedSubstrateTemplateActor(t *testing.T, ctx context.Context, persistence store.Interface, actorRef resources.ActorRef, actorState ateapipb.ActorState, mutations ...func(*ateapipb.ActorTemplate)) (*ActorWorkflow, *ateapipb.ObjectRef) {
	t.Helper()
	tmpl := validActorTemplate(mutations...)
	if _, err := persistence.CreateActorTemplate(ctx, tmpl); err != nil {
		t.Fatalf("seed actor template: %v", err)
	}
	templateRef := &ateapipb.ObjectRef{Atespace: tmpl.GetMetadata().GetAtespace(), Name: tmpl.GetMetadata().GetName()}
	seedWorkflowActor(t, ctx, persistence, actorRef, "", "", actorState, func(a *ateapipb.Actor) {
		a.ActorTemplate = templateRef
	})
	return &ActorWorkflow{store: persistence}, templateRef
}

// TestLoadActorForSuspend_SubstrateTemplateRef proves the suspend path serves
// substrate-ref actors: the store template is resolved and converted, so the
// snapshot location comes from the proto SnapshotsConfig.
func TestLoadActorForSuspend_SubstrateTemplateRef(t *testing.T) {
	ctx := context.Background()
	persistence := newTestPersistence(t)
	actorRef := resources.ActorRef{Atespace: "team-a", Name: "actor-1"}

	w, templateRef := seedSubstrateTemplateActor(t, ctx, persistence, actorRef, ateapipb.ActorState_ACTOR_STATE_RUNNING)

	actor, tmpl, err := w.loadActorForSuspend(ctx, actorRef)
	if err != nil {
		t.Fatalf("loadActorForSuspend: %v", err)
	}
	if got, want := tmpl.GetNamespace()+"/"+tmpl.GetName(), templateRef.GetAtespace()+"/"+templateRef.GetName(); got != want {
		t.Errorf("resolved template = %q, want %q", got, want)
	}
	if got, want := tmpl.Spec.SnapshotsConfig.Location, "gs://my-bucket/snapshots"; got != want {
		t.Errorf("SnapshotsConfig.Location = %q, want %q", got, want)
	}

	// The converted template feeds the same snapshot-location minting the
	// CRD path uses.
	marked, err := w.ensureMarkedSuspending(ctx, actorRef, actor, tmpl)
	if err != nil {
		t.Fatalf("ensureMarkedSuspending: %v", err)
	}
	if name := marked.GetStatus().GetInProgressSnapshotName(); !resources.IsValidResourceName(name) {
		t.Errorf("in-progress snapshot = %q, want a valid resource name", name)
	}
}

// TestLoadActorForResume_SubstrateTemplateGolden proves a substrate-ref actor
// with no snapshot of its own falls back to the golden snapshot named by the
// template's status golden_snapshot ObjectRef.
func TestLoadActorForResume_SubstrateTemplateGolden(t *testing.T) {
	ctx := context.Background()
	persistence := newTestPersistence(t)
	actorRef := resources.ActorRef{Atespace: "team-a", Name: "actor-1"}

	goldenURI := "gs://my-bucket/snapshots/snapshots/" + resources.GoldenActorAtespace + "/golden-1"
	if _, err := persistence.CreateActorSnapshot(ctx, &ateapipb.ActorSnapshot{
		Metadata: &ateapipb.ResourceMetadata{Atespace: resources.GoldenActorAtespace, Name: "golden-1"},
		Status: &ateapipb.ActorSnapshotStatus{
			ContentScope: ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL,
			SnapshotUri:  goldenURI,
		},
	}); err != nil {
		t.Fatalf("CreateActorSnapshot(golden): %v", err)
	}

	w, _ := seedSubstrateTemplateActor(t, ctx, persistence, actorRef, ateapipb.ActorState_ACTOR_STATE_SUSPENDED, func(tmpl *ateapipb.ActorTemplate) {
		tmpl.Status = &ateapipb.ActorTemplateStatus{
			Phase:          ateapipb.ActorTemplatePhase_ACTOR_TEMPLATE_PHASE_READY,
			GoldenSnapshot: &ateapipb.ObjectRef{Atespace: resources.GoldenActorAtespace, Name: "golden-1"},
		}
	})

	_, _, src, err := w.loadActorForResume(ctx, actorRef, false)
	if err != nil {
		t.Fatalf("loadActorForResume: %v", err)
	}
	if got := src.SnapshotURI.String(); got != goldenURI {
		t.Errorf("src.SnapshotURI = %q, want the golden snapshot URI %q", got, goldenURI)
	}
}

// TestLoadActorForResume_SubstrateTemplateFrozenAssets proves the frozen
// sandbox assets on a store template's status travel into the resolved boot
// source, so the boot path can use them instead of the pool's SandboxConfig.
func TestLoadActorForResume_SubstrateTemplateFrozenAssets(t *testing.T) {
	ctx := context.Background()
	persistence := newTestPersistence(t)
	actorRef := resources.ActorRef{Atespace: "team-a", Name: "actor-1"}

	frozen := &ateapipb.SandboxAssets{
		SandboxClass: ateapipb.SandboxClass_SANDBOX_CLASS_GVISOR,
		PauseImage:   "registry.k8s.io/pause@sha256:frozen",
		Assets: map[string]*ateapipb.ArchAssets{
			"amd64": {Files: map[string]*ateapipb.AssetFile{
				"gvisor": {Url: "gs://bucket/gvisor.tar.bz2", Sha256: "abc"},
			}},
		},
	}
	w, _ := seedSubstrateTemplateActor(t, ctx, persistence, actorRef, ateapipb.ActorState_ACTOR_STATE_SUSPENDED, func(tmpl *ateapipb.ActorTemplate) {
		tmpl.Status = &ateapipb.ActorTemplateStatus{
			Phase:         ateapipb.ActorTemplatePhase_ACTOR_TEMPLATE_PHASE_READY,
			SandboxAssets: frozen,
		}
	})

	_, _, src, err := w.loadActorForResume(ctx, actorRef, false)
	if err != nil {
		t.Fatalf("loadActorForResume: %v", err)
	}
	if diff := cmp.Diff(frozen, src.FrozenSandboxAssets, protocmp.Transform()); diff != "" {
		t.Errorf("src.FrozenSandboxAssets mismatch (-want +got):\n%s", diff)
	}
}

func TestLoadActorForResume_SubstrateTemplateNotFound(t *testing.T) {
	ctx := context.Background()
	persistence := newTestPersistence(t)
	actorRef := resources.ActorRef{Atespace: "team-a", Name: "actor-1"}

	seedWorkflowActor(t, ctx, persistence, actorRef, "", "", ateapipb.ActorState_ACTOR_STATE_SUSPENDED, func(a *ateapipb.Actor) {
		a.ActorTemplate = &ateapipb.ObjectRef{Atespace: "team-a", Name: "missing"}
	})
	w := &ActorWorkflow{store: persistence}

	_, _, _, err := w.loadActorForResume(ctx, actorRef, false)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("loadActorForResume = %v, want wrapped store.ErrNotFound", err)
	}
}

func TestTemplateWireRef(t *testing.T) {
	substrate := &ateapipb.Actor{
		ActorTemplate: &ateapipb.ObjectRef{Atespace: "ns1", Name: "tmpl-a"},
	}
	if ns, name := templateWireRef(substrate); ns != "ns1" || name != "tmpl-a" {
		t.Errorf("templateWireRef(substrate) = (%q, %q), want (ns1, tmpl-a)", ns, name)
	}

	legacy := &ateapipb.Actor{
		ActorTemplateNamespace: "default",
		ActorTemplateName:      "tmpl-b",
	}
	if ns, name := templateWireRef(legacy); ns != "default" || name != "tmpl-b" {
		t.Errorf("templateWireRef(legacy) = (%q, %q), want (default, tmpl-b)", ns, name)
	}
}
