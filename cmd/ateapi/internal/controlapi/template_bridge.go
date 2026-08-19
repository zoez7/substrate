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
	"fmt"

	"github.com/agent-substrate/substrate/internal/resources"
	atev1alpha1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
	listersv1alpha1 "github.com/agent-substrate/substrate/pkg/client/listers/api/v1alpha1"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type actorTemplateGetter interface {
	GetActorTemplate(ctx context.Context, templateRef resources.ActorTemplateRef) (*ateapipb.ActorTemplate, error)
}

// resolveActorTemplate loads the actor's template from whichever reference
// the actor carries: the substrate store ObjectRef, or the legacy CRD lister.
// Not-found errors pass through unmapped (store or k8s flavored) for the
// caller to translate.
func resolveActorTemplate(ctx context.Context, st actorTemplateGetter, lister listersv1alpha1.ActorTemplateLister, actor *ateapipb.Actor) (*atev1alpha1.ActorTemplate, error) {
	if ref := actor.GetActorTemplate(); ref != nil {
		stored, err := st.GetActorTemplate(ctx, resources.ActorTemplateRefFromObjectRef(ref))
		if err != nil {
			return nil, err
		}
		return actorTemplateFromProto(stored)
	}
	return lister.ActorTemplates(actor.GetActorTemplateNamespace()).Get(actor.GetActorTemplateName())
}

// actorTemplateFromProto builds an in-memory CRD-shaped ActorTemplate from a
// substrate-store template so the actor workflow (typed on the CRD struct)
// can serve actors of either template kind. Never persisted to Kubernetes.
// Only the fields the workflow reads are populated; kubebuilder defaults the
// API server would apply to a stored CRD are applied here instead.
func actorTemplateFromProto(t *ateapipb.ActorTemplate) (*atev1alpha1.ActorTemplate, error) {
	out := &atev1alpha1.ActorTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: t.GetMetadata().GetAtespace(),
			Name:      t.GetMetadata().GetName(),
			UID:       types.UID(t.GetMetadata().GetUid()),
		},
		Status: atev1alpha1.ActorTemplateStatus{
			GoldenSnapshot: t.GetStatus().GetGoldenSnapshot().GetName(),
		},
	}

	for _, c := range t.GetContainers() {
		ctr := atev1alpha1.Container{
			Name:    c.GetName(),
			Image:   c.GetImage(),
			Command: c.GetCommand(),
			Args:    c.GetArgs(),
		}
		for _, e := range c.GetEnv() {
			ctr.Env = append(ctr.Env, atev1alpha1.EnvVar{Name: e.GetName(), Value: e.GetValue()})
		}
		if r := c.GetReadyz(); r != nil {
			if r.GetHttpGet() == nil {
				return nil, fmt.Errorf("container %q: readyz requires http_get", c.GetName())
			}
			ctr.Readyz = &atev1alpha1.ContainerReadyz{
				HTTPGet: &atev1alpha1.HTTPGetAction{
					Path: r.GetHttpGet().GetPath(),
					Port: r.GetHttpGet().GetPort(),
				},
				TimeoutSeconds: r.GetTimeoutSeconds(),
			}
			if ctr.Readyz.HTTPGet.Path == "" {
				ctr.Readyz.HTTPGet.Path = "/readyz"
			}
			if ctr.Readyz.TimeoutSeconds == 0 {
				ctr.Readyz.TimeoutSeconds = 30
			}
		}
		for _, m := range c.GetVolumeMounts() {
			ctr.VolumeMounts = append(ctr.VolumeMounts, atev1alpha1.VolumeMount{
				Name:      m.GetName(),
				MountPath: m.GetMountPath(),
			})
		}
		out.Spec.Containers = append(out.Spec.Containers, ctr)
	}

	for _, v := range t.GetVolumes() {
		vol := atev1alpha1.Volume{Name: v.GetName()}
		switch {
		case v.GetDurableDir() != nil && v.GetExternalVolumeTemplate() != nil:
			return nil, fmt.Errorf("volume %q: exactly one of durable_dir or external_volume_template must be set", v.GetName())
		case v.GetDurableDir() != nil:
			vol.DurableDir = &atev1alpha1.DurableDirVolumeSource{}
		case v.GetExternalVolumeTemplate() != nil:
			capacity, err := resource.ParseQuantity(v.GetExternalVolumeTemplate().GetCapacity())
			if err != nil {
				return nil, fmt.Errorf("volume %q: invalid capacity %q: %w", v.GetName(), v.GetExternalVolumeTemplate().GetCapacity(), err)
			}
			vol.ExternalVolumeTemplate = &atev1alpha1.ExternalVolumeTemplate{
				Capacity:         capacity,
				StorageClassName: v.GetExternalVolumeTemplate().GetStorageClassName(),
			}
		default:
			return nil, fmt.Errorf("volume %q: exactly one of durable_dir or external_volume_template must be set", v.GetName())
		}
		out.Spec.Volumes = append(out.Spec.Volumes, vol)
	}

	sc := t.GetSnapshotsConfig()
	if sc == nil {
		return nil, fmt.Errorf("snapshots_config is required")
	}
	onPause, err := snapshotScopeFromProto(sc.GetOnPause())
	if err != nil {
		return nil, fmt.Errorf("snapshots_config.on_pause: %w", err)
	}
	onCommit, err := snapshotScopeFromProto(sc.GetOnCommit())
	if err != nil {
		return nil, fmt.Errorf("snapshots_config.on_commit: %w", err)
	}
	fromData, err := resumeSourceFromProto(sc.GetOnResume().GetFromData())
	if err != nil {
		return nil, fmt.Errorf("snapshots_config.on_resume.from_data: %w", err)
	}
	out.Spec.SnapshotsConfig = atev1alpha1.SnapshotsConfig{
		Location: sc.GetStorageLocation(),
		OnPause:  onPause,
		OnCommit: onCommit,
		OnResume: atev1alpha1.OnResumeConfig{FromData: fromData},
	}

	// SandboxConfig.config_name has no CRD-spec counterpart and is unused by
	// the workflow (cold-boot assets resolve from the WorkerPool), so only
	// the class carries over.
	switch class := t.GetSandboxConfig().GetSandboxClass(); class {
	case ateapipb.SandboxClass_SANDBOX_CLASS_GVISOR:
		out.Spec.SandboxClass = atev1alpha1.SandboxClassGvisor
	case ateapipb.SandboxClass_SANDBOX_CLASS_MICROVM:
		out.Spec.SandboxClass = atev1alpha1.SandboxClassMicroVM
	default:
		return nil, fmt.Errorf("sandbox_config.sandbox_class: unsupported value %v", class)
	}

	if sel := t.GetWorkerSelector(); sel != nil {
		out.Spec.WorkerSelector = &metav1.LabelSelector{MatchLabels: sel.GetMatchLabels()}
	}

	if limits := t.GetResources().GetLimits(); len(limits) > 0 {
		list := corev1.ResourceList{}
		for _, l := range limits {
			quantity, err := resource.ParseQuantity(l.GetQuantity())
			if err != nil {
				return nil, fmt.Errorf("resources.limits[%q]: invalid quantity %q: %w", l.GetName(), l.GetQuantity(), err)
			}
			list[corev1.ResourceName(l.GetName())] = quantity
		}
		out.Spec.Resources = &corev1.ResourceRequirements{Limits: list}
	}

	return out, nil
}

func snapshotScopeFromProto(s ateapipb.SnapshotContentScope) (atev1alpha1.SnapshotScope, error) {
	switch s {
	case ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_UNSPECIFIED,
		ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL:
		return atev1alpha1.SnapshotScopeFull, nil
	case ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA:
		return atev1alpha1.SnapshotScopeData, nil
	default:
		return "", fmt.Errorf("unsupported value %v", s)
	}
}

func resumeSourceFromProto(s ateapipb.ResumeSource) (atev1alpha1.ResumeSource, error) {
	switch s {
	case ateapipb.ResumeSource_RESUME_SOURCE_UNSPECIFIED,
		ateapipb.ResumeSource_RESUME_SOURCE_COLD_BOOT:
		return atev1alpha1.ResumeSourceColdBoot, nil
	case ateapipb.ResumeSource_RESUME_SOURCE_GOLDEN:
		return atev1alpha1.ResumeSourceGolden, nil
	default:
		return "", fmt.Errorf("unsupported value %v", s)
	}
}

// templateWireRef returns the (namespace, name) pair stamped into atelet
// requests, worker assignments, snapshot records, and metric attributes:
// the substrate ObjectRef's (atespace, name) when the actor carries one,
// the legacy CRD (namespace, name) otherwise.
func templateWireRef(actor *ateapipb.Actor) (namespace, name string) {
	if ref := actor.GetActorTemplate(); ref != nil {
		return ref.GetAtespace(), ref.GetName()
	}
	return actor.GetActorTemplateNamespace(), actor.GetActorTemplateName()
}
