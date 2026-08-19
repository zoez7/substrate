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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/cmd/ateapi/internal/store"
	"github.com/agent-substrate/substrate/internal/resources"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/util/wait"
)

// fakeGoldenControl is an in-memory goldenActorControl that records calls,
// returns configured errors, and lets tests inject concurrent writes via
// hooks.
type fakeGoldenControl struct {
	mu           sync.Mutex
	createCalls  int
	resumeCalls  int
	suspendCalls int

	createErr  error
	resumeErr  error
	suspendErr error

	getActorState ateapipb.ActorState

	onCreateActor func()
}

func (f *fakeGoldenControl) CreateActor(ctx context.Context, req *ateapipb.CreateActorRequest) (*ateapipb.Actor, error) {
	f.mu.Lock()
	f.createCalls++
	err := f.createErr
	hook := f.onCreateActor
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	if err != nil {
		return nil, err
	}
	return req.GetActor(), nil
}

func (f *fakeGoldenControl) GetActor(ctx context.Context, req *ateapipb.GetActorRequest) (*ateapipb.Actor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &ateapipb.Actor{Status: &ateapipb.ActorStatus{State: f.getActorState}}, nil
}

func (f *fakeGoldenControl) ResumeActor(ctx context.Context, req *ateapipb.ResumeActorRequest) (*ateapipb.ResumeActorResponse, error) {
	f.mu.Lock()
	f.resumeCalls++
	err := f.resumeErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &ateapipb.ResumeActorResponse{}, nil
}

func (f *fakeGoldenControl) SuspendActor(ctx context.Context, req *ateapipb.SuspendActorRequest) (*ateapipb.SuspendActorResponse, error) {
	f.mu.Lock()
	f.suspendCalls++
	err := f.suspendErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &ateapipb.SuspendActorResponse{Actor: &ateapipb.Actor{
		Status: &ateapipb.ActorStatus{LatestSnapshot: &ateapipb.ObjectRef{Atespace: "ns1", Name: "golden-snap"}},
	}}, nil
}

func (f *fakeGoldenControl) calls() (create, resume, suspend int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createCalls, f.resumeCalls, f.suspendCalls
}

// withReadyz gives every container a readyz probe so goldenSnapshotWarmupFor
// reports a zero warmup and the whole state machine runs in one pass.
func withReadyz(tmpl *ateapipb.ActorTemplate) {
	for _, c := range tmpl.Containers {
		c.Readyz = &ateapipb.ContainerReadyz{}
	}
}

// seedTemplate stores a minimal valid template at the given phase and returns
// its ref. Mutations run after the phase is set so they can touch Status too.
func seedTemplate(t *testing.T, persistence store.Interface, phase ateapipb.ActorTemplatePhase, mutations ...func(*ateapipb.ActorTemplate)) resources.ActorTemplateRef {
	t.Helper()
	tmpl := validActorTemplate()
	tmpl.Status = &ateapipb.ActorTemplateStatus{Phase: phase}
	for _, m := range mutations {
		m(tmpl)
	}
	if _, err := persistence.CreateActorTemplate(t.Context(), tmpl); err != nil {
		t.Fatalf("CreateActorTemplate: %v", err)
	}
	return resources.ActorTemplateRefFromActorTemplate(tmpl)
}

func getPhase(t *testing.T, persistence store.Interface, ref resources.ActorTemplateRef) *ateapipb.ActorTemplate {
	t.Helper()
	tmpl, err := persistence.GetActorTemplate(t.Context(), ref)
	if err != nil {
		t.Fatalf("GetActorTemplate: %v", err)
	}
	return tmpl
}

func TestTemplateReconcileHappyPathZeroWarmup(t *testing.T) {
	ctx := t.Context()
	persistence := newTestPersistence(t)
	control := &fakeGoldenControl{}
	r := NewActorTemplateReconciler(persistence, control)
	ref := seedTemplate(t, persistence, ateapipb.ActorTemplatePhase_ACTOR_TEMPLATE_PHASE_INITIAL, withReadyz)

	requeueAfter, err := r.reconcileOne(ctx, ref)
	if err != nil || requeueAfter != 0 {
		t.Fatalf("reconcileOne = (%v, %v), want (0, nil)", requeueAfter, err)
	}

	tmpl := getPhase(t, persistence, ref)
	if got := tmpl.GetStatus().GetPhase(); got != ateapipb.ActorTemplatePhase_ACTOR_TEMPLATE_PHASE_READY {
		t.Errorf("phase = %v, want READY", got)
	}
	if got := tmpl.GetStatus().GetGoldenSnapshot().GetName(); got != "golden-snap" {
		t.Errorf("golden snapshot = %q, want %q", got, "golden-snap")
	}
	if create, resume, suspend := control.calls(); create != 1 || resume != 1 || suspend != 1 {
		t.Errorf("control calls = (%d, %d, %d), want (1, 1, 1)", create, resume, suspend)
	}
}

func TestTemplateReconcileWarmupRequestsRequeue(t *testing.T) {
	ctx := t.Context()
	persistence := newTestPersistence(t)
	control := &fakeGoldenControl{}
	r := NewActorTemplateReconciler(persistence, control)
	ref := seedTemplate(t, persistence, ateapipb.ActorTemplatePhase_ACTOR_TEMPLATE_PHASE_INITIAL)

	requeueAfter, err := r.reconcileOne(ctx, ref)
	if err != nil {
		t.Fatalf("reconcileOne: %v", err)
	}
	if requeueAfter <= 0 || requeueAfter > goldenSnapshotWarmup {
		t.Errorf("requeueAfter = %v, want in (0, %v]", requeueAfter, goldenSnapshotWarmup)
	}

	tmpl := getPhase(t, persistence, ref)
	if got := tmpl.GetStatus().GetPhase(); got != ateapipb.ActorTemplatePhase_ACTOR_TEMPLATE_PHASE_WAIT_GOLDEN_ACTOR {
		t.Errorf("phase = %v, want WAIT_GOLDEN_ACTOR", got)
	}
	if tmpl.GetStatus().GetTakeGoldenSnapshotAt() == nil {
		t.Error("TakeGoldenSnapshotAt not set")
	}
	if _, _, suspend := control.calls(); suspend != 0 {
		t.Errorf("suspend called %d times during warmup, want 0", suspend)
	}
}

func TestTemplateReconcileWaitDeadlinePassed(t *testing.T) {
	ctx := t.Context()
	persistence := newTestPersistence(t)
	control := &fakeGoldenControl{}
	r := NewActorTemplateReconciler(persistence, control)
	ref := seedTemplate(t, persistence, ateapipb.ActorTemplatePhase_ACTOR_TEMPLATE_PHASE_WAIT_GOLDEN_ACTOR, func(tmpl *ateapipb.ActorTemplate) {
		tmpl.Status.TakeGoldenSnapshotAt = timestamppb.New(time.Now().Add(-time.Second))
	})

	requeueAfter, err := r.reconcileOne(ctx, ref)
	if err != nil || requeueAfter != 0 {
		t.Fatalf("reconcileOne = (%v, %v), want (0, nil)", requeueAfter, err)
	}
	if got := getPhase(t, persistence, ref).GetStatus().GetPhase(); got != ateapipb.ActorTemplatePhase_ACTOR_TEMPLATE_PHASE_READY {
		t.Errorf("phase = %v, want READY", got)
	}
}

func TestTemplateReconcileErrorRequeuesRateLimited(t *testing.T) {
	ctx := t.Context()
	persistence := newTestPersistence(t)
	control := &fakeGoldenControl{resumeErr: status.Error(codes.Unavailable, "atelet down")}
	r := NewActorTemplateReconciler(persistence, control)
	ref := seedTemplate(t, persistence, ateapipb.ActorTemplatePhase_ACTOR_TEMPLATE_PHASE_INITIAL, withReadyz)

	r.queue.Add(ref)
	if !r.processNextWorkItem(ctx) {
		t.Fatal("processNextWorkItem returned quit")
	}
	// The rate limiter re-adds the key after a short backoff.
	if err := wait.PollUntilContextTimeout(ctx, 10*time.Millisecond, 2*time.Second, true, func(context.Context) (bool, error) {
		return r.queue.Len() == 1, nil
	}); err != nil {
		t.Fatalf("key was not requeued after error: %v", err)
	}
	// The transition before the failing resume was committed.
	if got := getPhase(t, persistence, ref).GetStatus().GetPhase(); got != ateapipb.ActorTemplatePhase_ACTOR_TEMPLATE_PHASE_RESUME_GOLDEN_ACTOR {
		t.Errorf("phase = %v, want RESUME_GOLDEN_ACTOR", got)
	}
}

func TestTemplateReconcileLockConflictDrops(t *testing.T) {
	ctx := t.Context()
	persistence := newTestPersistence(t)
	control := &fakeGoldenControl{}
	r := NewActorTemplateReconciler(persistence, control)
	ref := seedTemplate(t, persistence, ateapipb.ActorTemplatePhase_ACTOR_TEMPLATE_PHASE_INITIAL, withReadyz)

	lock, err := persistence.AcquireLock(ctx, "lock:actortemplate:"+ref.Atespace+":"+ref.Name)
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	defer lock.Close()

	requeueAfter, err := r.reconcileOne(ctx, ref)
	if err != nil || requeueAfter != 0 {
		t.Fatalf("reconcileOne = (%v, %v), want (0, nil)", requeueAfter, err)
	}
	if got := getPhase(t, persistence, ref).GetStatus().GetPhase(); got != ateapipb.ActorTemplatePhase_ACTOR_TEMPLATE_PHASE_INITIAL {
		t.Errorf("phase = %v, want INITIAL (untouched)", got)
	}
	if create, _, _ := control.calls(); create != 0 {
		t.Errorf("CreateActor called %d times under a held lock, want 0", create)
	}
}

func TestTemplateReconcileStalePhaseDrops(t *testing.T) {
	ctx := t.Context()
	persistence := newTestPersistence(t)
	control := &fakeGoldenControl{}
	r := NewActorTemplateReconciler(persistence, control)
	ref := seedTemplate(t, persistence, ateapipb.ActorTemplatePhase_ACTOR_TEMPLATE_PHASE_INITIAL, withReadyz)

	// A concurrent writer flips the phase between the actor op and the
	// checkpoint; the preconditioned write must lose and be dropped, which
	// surfaces as an error so the key is requeued and re-observed.
	control.onCreateActor = func() {
		_, err := persistence.UpdateActorTemplate(ctx, ref, func(dbTemplate *ateapipb.ActorTemplate) error {
			dbTemplate.Status.Phase = ateapipb.ActorTemplatePhase_ACTOR_TEMPLATE_PHASE_FAILED
			dbTemplate.Status.Message = "concurrent writer"
			return nil
		})
		if err != nil {
			t.Errorf("concurrent UpdateActorTemplate: %v", err)
		}
	}

	requeueAfter, err := r.reconcileOne(ctx, ref)
	if err == nil {
		t.Fatal("reconcileOne returned nil error, want error for dropped checkpoint")
	}
	if requeueAfter != 0 {
		t.Fatalf("requeueAfter = %v, want 0", requeueAfter)
	}
	tmpl := getPhase(t, persistence, ref)
	if got := tmpl.GetStatus().GetPhase(); got != ateapipb.ActorTemplatePhase_ACTOR_TEMPLATE_PHASE_FAILED {
		t.Errorf("phase = %v, want FAILED (concurrent writer wins)", got)
	}
	if got := tmpl.GetStatus().GetMessage(); got != "concurrent writer" {
		t.Errorf("message = %q, want %q", got, "concurrent writer")
	}
}

func TestTemplateReconcileInvalidArgumentFails(t *testing.T) {
	ctx := t.Context()
	persistence := newTestPersistence(t)
	control := &fakeGoldenControl{createErr: status.Error(codes.InvalidArgument, "bad image")}
	r := NewActorTemplateReconciler(persistence, control)
	ref := seedTemplate(t, persistence, ateapipb.ActorTemplatePhase_ACTOR_TEMPLATE_PHASE_INITIAL, withReadyz)

	requeueAfter, err := r.reconcileOne(ctx, ref)
	if err != nil || requeueAfter != 0 {
		t.Fatalf("reconcileOne = (%v, %v), want (0, nil)", requeueAfter, err)
	}
	tmpl := getPhase(t, persistence, ref)
	if got := tmpl.GetStatus().GetPhase(); got != ateapipb.ActorTemplatePhase_ACTOR_TEMPLATE_PHASE_FAILED {
		t.Errorf("phase = %v, want FAILED", got)
	}
	if got := tmpl.GetStatus().GetMessage(); !strings.Contains(got, "creating golden actor") {
		t.Errorf("message = %q, want it to mention creating golden actor", got)
	}
}

func TestTemplateResyncEnqueues(t *testing.T) {
	ctx := t.Context()
	persistence := newTestPersistence(t)
	control := &fakeGoldenControl{}
	r := NewActorTemplateReconciler(persistence, control)

	const futureDelay = 500 * time.Millisecond
	phases := []struct {
		name  string
		phase ateapipb.ActorTemplatePhase
		mut   func(*ateapipb.ActorTemplate)
	}{
		{"tmpl-ready", ateapipb.ActorTemplatePhase_ACTOR_TEMPLATE_PHASE_READY, nil},
		{"tmpl-failed", ateapipb.ActorTemplatePhase_ACTOR_TEMPLATE_PHASE_FAILED, nil},
		{"tmpl-initial", ateapipb.ActorTemplatePhase_ACTOR_TEMPLATE_PHASE_INITIAL, nil},
		{"tmpl-wait-past", ateapipb.ActorTemplatePhase_ACTOR_TEMPLATE_PHASE_WAIT_GOLDEN_ACTOR, func(tmpl *ateapipb.ActorTemplate) {
			tmpl.Status.TakeGoldenSnapshotAt = timestamppb.New(time.Now().Add(-time.Second))
		}},
		{"tmpl-wait-future", ateapipb.ActorTemplatePhase_ACTOR_TEMPLATE_PHASE_WAIT_GOLDEN_ACTOR, func(tmpl *ateapipb.ActorTemplate) {
			tmpl.Status.TakeGoldenSnapshotAt = timestamppb.New(time.Now().Add(futureDelay))
		}},
	}
	for _, p := range phases {
		muts := []func(*ateapipb.ActorTemplate){func(tmpl *ateapipb.ActorTemplate) { tmpl.Metadata.Name = p.name }}
		if p.mut != nil {
			muts = append(muts, p.mut)
		}
		seedTemplate(t, persistence, p.phase, muts...)
	}

	r.resync(ctx)

	// Every non-terminal template enqueues immediately; the future WAIT
	// template's snapshot delay is handled by the worker, not by resync.
	if got := r.queue.Len(); got != 3 {
		t.Errorf("queue.Len() right after resync = %d, want 3", got)
	}
}

func TestTemplateReconcilerStartDrivesTemplateToReady(t *testing.T) {
	ctx := t.Context()
	persistence := newTestPersistence(t)
	control := &fakeGoldenControl{}
	r := NewActorTemplateReconciler(persistence, control)
	ref := seedTemplate(t, persistence, ateapipb.ActorTemplatePhase_ACTOR_TEMPLATE_PHASE_INITIAL, withReadyz)

	r.Start(ctx)

	if err := wait.PollUntilContextTimeout(ctx, 20*time.Millisecond, 5*time.Second, true, func(context.Context) (bool, error) {
		tmpl, err := persistence.GetActorTemplate(ctx, ref)
		if err != nil {
			return false, err
		}
		return tmpl.GetStatus().GetPhase() == ateapipb.ActorTemplatePhase_ACTOR_TEMPLATE_PHASE_READY, nil
	}); err != nil {
		t.Fatalf("template never reached READY: %v", err)
	}
}
