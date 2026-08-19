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
	"fmt"
	"log/slog"
	"time"

	"github.com/agent-substrate/substrate/cmd/ateapi/internal/store"
	"github.com/agent-substrate/substrate/internal/resources"
	listersv1alpha1 "github.com/agent-substrate/substrate/pkg/client/listers/api/v1alpha1"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/workqueue"
)

const (
	templateResyncInterval = 5 * time.Second
	templateListPageSize   = 500

	// templateWorkerCount is the number of goroutines draining the work
	// queue.
	templateWorkerCount = 2

	// goldenSnapshotWarmup is the default wall-clock delay between resuming
	// the golden actor and taking its snapshot, for templates without a
	// readiness probe on every container. Mirrors the ActorTemplate CRD
	// controller (cmd/atecontroller/internal/controllers); keep in sync.
	goldenSnapshotWarmup = 20 * time.Second
)

// errStalePhase aborts a status transition whose phase precondition no longer
// holds; the template is re-observed on the next resync.
var errStalePhase = errors.New("actor template phase changed concurrently")

// templateReconcilerStore enumerates the exact storage methods needed by
// ActorTemplateReconciler and nothing more.
type templateReconcilerStore interface {
	GetActorTemplate(ctx context.Context, templateRef resources.ActorTemplateRef) (*ateapipb.ActorTemplate, error)
	ListActorTemplates(ctx context.Context, atespace string, opts store.ListOptions) (store.ListResponse[*ateapipb.ActorTemplate], error)
	UpdateActorTemplate(ctx context.Context, templateRef resources.ActorTemplateRef, mutate func(dbTemplate *ateapipb.ActorTemplate) error) (*ateapipb.ActorTemplate, error)
	AcquireLock(ctx context.Context, key string) (*store.Lock, error)
}

// goldenActorControl is the in-process slice of the Control service the
// reconciler drives golden actors through. *Service satisfies it.
type goldenActorControl interface {
	CreateActor(ctx context.Context, req *ateapipb.CreateActorRequest) (*ateapipb.Actor, error)
	GetActor(ctx context.Context, req *ateapipb.GetActorRequest) (*ateapipb.Actor, error)
	ResumeActor(ctx context.Context, req *ateapipb.ResumeActorRequest) (*ateapipb.ResumeActorResponse, error)
	SuspendActor(ctx context.Context, req *ateapipb.SuspendActorRequest) (*ateapipb.SuspendActorResponse, error)
}

// ActorTemplateReconciler drives stored ActorTemplates through the golden
// actor state machine (FREEZE_SANDBOX_CONFIG -> INITIAL ->
// RESUME_GOLDEN_ACTOR -> WAIT_GOLDEN_ACTOR -> READY), the substrate-resource
// equivalent of the ActorTemplate CRD controller. It runs in every ate-api
// replica: a per-template lock plus phase-preconditioned status writes and
// idempotent actor operations keep concurrent replicas safe.
type ActorTemplateReconciler struct {
	persistence    templateReconcilerStore
	control        goldenActorControl
	sandboxConfigs listersv1alpha1.SandboxConfigLister
	queue          workqueue.TypedRateLimitingInterface[resources.ActorTemplateRef]
}

func NewActorTemplateReconciler(persistence templateReconcilerStore, control goldenActorControl, sandboxConfigs listersv1alpha1.SandboxConfigLister) *ActorTemplateReconciler {
	return &ActorTemplateReconciler{
		persistence:    persistence,
		control:        control,
		sandboxConfigs: sandboxConfigs,
		queue:          workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[resources.ActorTemplateRef]()),
	}
}

// Start launches the queue workers and the resync producer; there is no event
// source for stored templates, so the periodic list is the event source.
func (r *ActorTemplateReconciler) Start(ctx context.Context) {
	go func() {
		defer r.queue.ShutDown()
		for range templateWorkerCount {
			go wait.UntilWithContext(ctx, r.runWorker, time.Second)
		}
		wait.UntilWithContext(ctx, r.resync, templateResyncInterval)
	}()
}

func (r *ActorTemplateReconciler) resync(ctx context.Context) {
	pageToken := ""
	for {
		// TODO: need sharding
		page, err := r.persistence.ListActorTemplates(ctx, "", store.ListOptions{PageSize: templateListPageSize, PageToken: pageToken})
		if err != nil {
			slog.ErrorContext(ctx, "Failed to list actor templates", slog.Any("err", err))
			return
		}
		for _, tmpl := range page.Items {
			if needsWork(tmpl) {
				r.queue.Add(resources.ActorTemplateRefFromActorTemplate(tmpl))
			}
		}
		if page.NextPageToken == "" {
			return
		}
		pageToken = page.NextPageToken
	}
}

func (r *ActorTemplateReconciler) runWorker(ctx context.Context) {
	for r.processNextWorkItem(ctx) {
	}
}

func (r *ActorTemplateReconciler) processNextWorkItem(ctx context.Context) bool {
	ref, quit := r.queue.Get()
	if quit {
		return false
	}
	defer r.queue.Done(ref)

	requeueAfter, err := r.reconcileOne(ctx, ref)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to reconcile actor template, requeueing",
			slog.Any("template", ref),
			slog.Any("err", err))
		r.queue.AddRateLimited(ref)
		return true
	}
	r.queue.Forget(ref)
	if requeueAfter > 0 {
		r.queue.AddAfter(ref, requeueAfter)
	}
	return true
}

// needsWork reports whether tmpl is in a phase the reconciler can still
// advance; reconcileOne arms the precise requeue for templates whose snapshot
// deadline has not passed yet.
func needsWork(tmpl *ateapipb.ActorTemplate) bool {
	switch tmpl.GetStatus().GetPhase() {
	case ateapipb.ActorTemplatePhase_ACTOR_TEMPLATE_PHASE_READY,
		ateapipb.ActorTemplatePhase_ACTOR_TEMPLATE_PHASE_FAILED,
		ateapipb.ActorTemplatePhase_ACTOR_TEMPLATE_PHASE_UNSPECIFIED:
		return false
	}
	return true
}

// reconcileOne advances one template as far as it can go in this pass, holding
// the template's lock so concurrent replicas don't interleave transitions. A
// positive requeueAfter asks the caller to revisit the template once its
// snapshot deadline passes.
func (r *ActorTemplateReconciler) reconcileOne(ctx context.Context, ref resources.ActorTemplateRef) (requeueAfter time.Duration, err error) {
	lock, err := r.persistence.AcquireLock(ctx, "lock:actortemplate:"+ref.Atespace+":"+ref.Name)
	if err != nil {
		if errors.Is(err, store.ErrLockConflict) {
			// Another replica owns this template for now.
			return 0, nil
		}
		return 0, fmt.Errorf("while acquiring lock: %w", err)
	}
	defer lock.Close()
	ctx = lock.Context()

	tmpl, err := r.persistence.GetActorTemplate(ctx, ref)
	if err != nil {
		// Template was deleted after we enqueued the work item.
		if errors.Is(err, store.ErrNotFound) {
			return 0, nil
		}
		return 0, err
	}

	goldenRef := &ateapipb.ObjectRef{
		Atespace: tmpl.GetMetadata().GetAtespace(),
		Name:     tmpl.GetMetadata().GetName() + "-golden",
	}

	// Each iteration performs one phase's actor operation and checkpoints the
	// transition. A nil tmpl means a checkpoint was dropped on a stale phase;
	// stop and let the next resync re-observe the template.
	for {
		if tmpl == nil {
			return 0, fmt.Errorf("ActorTemplate is nil")
		}

		phase := tmpl.GetStatus().GetPhase()
		switch phase {
		case ateapipb.ActorTemplatePhase_ACTOR_TEMPLATE_PHASE_FREEZE_SANDBOX_CONFIG:
			// Freeze the current content of the referenced SandboxConfig into
			// status — before the golden actor exists — so later edits to the
			// config don't affect this template.
			class, ok := sandboxClassFromProto(tmpl.GetSandboxConfig().GetSandboxClass())
			if !ok {
				return 0, r.fail(ctx, ref, phase, fmt.Sprintf("unrecognized sandbox class %q", tmpl.GetSandboxConfig().GetSandboxClass()))
			}
			configName := tmpl.GetSandboxConfig().GetConfigName()
			sc, err := r.sandboxConfigs.Get(configName)
			if err != nil {
				if apierrors.IsNotFound(err) {
					return 0, r.fail(ctx, ref, phase, fmt.Sprintf("freezing sandbox config: SandboxConfig %q not found", configName))
				}
				return 0, fmt.Errorf("while getting SandboxConfig %q: %w", configName, err)
			}
			if sc.Spec.SandboxClass != class {
				return 0, r.fail(ctx, ref, phase, fmt.Sprintf("SandboxConfig %q has class %q but the template requires %q", configName, sc.Spec.SandboxClass, class))
			}

			assets := frozenSandboxAssetsProto(tmpl.GetSandboxConfig().GetSandboxClass(), sc)
			if tmpl, err = r.checkpoint(ctx, ref, phase, func(templateStatus *ateapipb.ActorTemplateStatus) {
				templateStatus.Phase = ateapipb.ActorTemplatePhase_ACTOR_TEMPLATE_PHASE_INITIAL
				templateStatus.SandboxAssets = assets
			}); err != nil {
				return 0, err
			}

		case ateapipb.ActorTemplatePhase_ACTOR_TEMPLATE_PHASE_INITIAL:
			if _, err := r.control.CreateActor(ctx, &ateapipb.CreateActorRequest{
				Actor: &ateapipb.Actor{
					Metadata: &ateapipb.ResourceMetadata{
						Atespace: goldenRef.GetAtespace(),
						Name:     goldenRef.GetName(),
					},
					ActorTemplate: ref.ToObjectRef(),
				},
			}); err != nil && status.Code(err) != codes.AlreadyExists {
				if status.Code(err) == codes.InvalidArgument {
					// Mark the actor template as failed because InvalidArgument cannot be retried.
					return 0, r.fail(ctx, ref, phase, fmt.Sprintf("creating golden actor: %v", err))
				}
				return 0, fmt.Errorf("while creating golden actor: %w", err)
			}
			if tmpl, err = r.checkpoint(ctx, ref, phase, func(templateStatus *ateapipb.ActorTemplateStatus) {
				templateStatus.Phase = ateapipb.ActorTemplatePhase_ACTOR_TEMPLATE_PHASE_RESUME_GOLDEN_ACTOR
			}); err != nil {
				return 0, err
			}

		case ateapipb.ActorTemplatePhase_ACTOR_TEMPLATE_PHASE_RESUME_GOLDEN_ACTOR:
			if _, err := r.control.ResumeActor(ctx, &ateapipb.ResumeActorRequest{Actor: goldenRef}); err != nil {
				if r.goldenCrashed(ctx, goldenRef) {
					return 0, r.fail(ctx, ref, phase, fmt.Sprintf("golden actor crashed while booting: %v", err))
				}
				return 0, fmt.Errorf("while resuming golden actor: %w", err)
			}

			takeAt := time.Now().Add(goldenSnapshotWarmupFor(tmpl.GetContainers()))
			if tmpl, err = r.checkpoint(ctx, ref, phase, func(templateStatus *ateapipb.ActorTemplateStatus) {
				templateStatus.Phase = ateapipb.ActorTemplatePhase_ACTOR_TEMPLATE_PHASE_WAIT_GOLDEN_ACTOR
				templateStatus.TakeGoldenSnapshotAt = timestamppb.New(takeAt)
			}); err != nil {
				return 0, err
			}

		case ateapipb.ActorTemplatePhase_ACTOR_TEMPLATE_PHASE_WAIT_GOLDEN_ACTOR:
			// Not time to take the golden snapshot yet; ask for a precise
			// requeue instead of waiting for a resync to rediscover the template.
			if takeAt := tmpl.GetStatus().GetTakeGoldenSnapshotAt(); takeAt != nil {
				if rem := time.Until(takeAt.AsTime()); rem > 0 {
					return rem, nil
				}
			}
			resp, err := r.control.SuspendActor(ctx, &ateapipb.SuspendActorRequest{Actor: goldenRef})
			if err != nil {
				if r.goldenCrashed(ctx, goldenRef) {
					return 0, r.fail(ctx, ref, phase, fmt.Sprintf("golden actor crashed while booting: %v", err))
				}
				return 0, fmt.Errorf("while suspending golden actor: %w", err)
			}
			snapshot := resp.GetActor().GetStatus().GetLatestSnapshot()
			if snapshot == nil {
				return 0, fmt.Errorf("suspending golden actor returned no ActorSnapshot")
			}

			if tmpl, err = r.checkpoint(ctx, ref, phase, func(templateStatus *ateapipb.ActorTemplateStatus) {
				templateStatus.Phase = ateapipb.ActorTemplatePhase_ACTOR_TEMPLATE_PHASE_READY
				templateStatus.GoldenSnapshot = snapshot
			}); err != nil {
				return 0, err
			}
		case ateapipb.ActorTemplatePhase_ACTOR_TEMPLATE_PHASE_READY,
			ateapipb.ActorTemplatePhase_ACTOR_TEMPLATE_PHASE_FAILED:
			return 0, nil
		default:
			return 0, fmt.Errorf("unrecognized phase %q", phase)
		}
	}
}

// checkpoint commits a status mutation preconditioned on the phase the
// actions above were taken for. A stale phase means another writer advanced
// the template concurrently; the checkpoint is dropped without error and the
// template is re-observed on the next resync.
func (r *ActorTemplateReconciler) checkpoint(ctx context.Context, ref resources.ActorTemplateRef, fromPhase ateapipb.ActorTemplatePhase, mutate func(*ateapipb.ActorTemplateStatus)) (*ateapipb.ActorTemplate, error) {
	updated, err := r.persistence.UpdateActorTemplate(ctx, ref, func(dbTemplate *ateapipb.ActorTemplate) error {
		if dbTemplate.GetStatus().GetPhase() != fromPhase {
			return errStalePhase
		}
		if dbTemplate.Status == nil {
			dbTemplate.Status = &ateapipb.ActorTemplateStatus{}
		}
		mutate(dbTemplate.Status)
		return nil
	})
	if errors.Is(err, errStalePhase) {
		return nil, nil
	}
	return updated, err
}

// fail commits the terminal FAILED phase with a human-readable message,
// preconditioned on fromPhase like every other transition.
func (r *ActorTemplateReconciler) fail(ctx context.Context, ref resources.ActorTemplateRef, fromPhase ateapipb.ActorTemplatePhase, msg string) error {
	_, err := r.checkpoint(ctx, ref, fromPhase, func(templateStatus *ateapipb.ActorTemplateStatus) {
		templateStatus.Phase = ateapipb.ActorTemplatePhase_ACTOR_TEMPLATE_PHASE_FAILED
		templateStatus.Message = msg
	})
	return err
}

// goldenCrashed reports whether the golden actor is terminally CRASHED. A
// GetActor error reads as "not crashed" so the caller surfaces its original
// error and retries, rather than failing the template on a blip.
func (r *ActorTemplateReconciler) goldenCrashed(ctx context.Context, goldenRef *ateapipb.ObjectRef) bool {
	actor, err := r.control.GetActor(ctx, &ateapipb.GetActorRequest{Actor: goldenRef})
	return err == nil && actor.GetStatus().GetState() == ateapipb.ActorState_ACTOR_STATE_CRASHED
}

// goldenSnapshotWarmupFor returns 0 when every container has a readyz probe
// (ResumeActor already blocked until the workload reported 200), and the
// default warmup otherwise. Mirrors the CRD controller's function of the
// same name; keep both in sync.
func goldenSnapshotWarmupFor(containers []*ateapipb.Container) time.Duration {
	if len(containers) == 0 {
		return goldenSnapshotWarmup
	}
	for _, container := range containers {
		if container.GetReadyz() == nil {
			return goldenSnapshotWarmup
		}
	}
	return 0
}
