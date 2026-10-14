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
	"github.com/agent-substrate/substrate/cmd/ateapi/internal/store/storetest"
	"github.com/agent-substrate/substrate/internal/apierror"
	"github.com/agent-substrate/substrate/internal/installdefaults"
	"github.com/agent-substrate/substrate/internal/objectstore/objectstoretest"
	"github.com/agent-substrate/substrate/internal/objectstoreplugin/objectstoreplugintest"
	"github.com/agent-substrate/substrate/internal/proto/ateletpb"
	"github.com/agent-substrate/substrate/internal/resources"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"
	"k8s.io/client-go/tools/cache"
)

func TestEnsureMarkedSuspending_SnapshotURI(t *testing.T) {
	ctx := context.Background()
	persistence := newTestPersistence(t)
	if _, err := persistence.CreateActorTemplate(ctx, &ateapipb.ActorTemplate{
		Metadata:       &ateapipb.ResourceMetadata{Atespace: "team-a", Name: "sub-tmpl"},
		SnapshotConfig: &ateapipb.SnapshotConfig{StorageLocation: "gs://bucket/root/"},
		SandboxConfig:  &ateapipb.SandboxConfig{SandboxClass: "gvisor", ConfigName: "gvisor"},
	}); err != nil {
		t.Fatalf("CreateActorTemplate: %v", err)
	}
	workerName := testWorkerUID("worker-pod-1")
	wantRuntime := &ateapipb.SandboxRuntime{
		SandboxClass: "gvisor",
		Version: &ateapipb.VersionedSandboxCompat{
			SchemaVersion: "v1",
			Attributes: []*ateapipb.AttributeEntry{
				{Key: "architecture", Value: "amd64"},
			},
		},
	}
	actorRef := resources.ActorRef{Atespace: "team-a", Name: "actor-1"}
	storetest.MustCreateActor(t, ctx, persistence, &ateapipb.Actor{
		Metadata:      &ateapipb.ResourceMetadata{Atespace: actorRef.Atespace, Name: actorRef.Name},
		ActorTemplate: &ateapipb.ObjectRef{Atespace: "team-a", Name: "sub-tmpl"},
		Status: &ateapipb.ActorStatus{
			State: ateapipb.ActorState_ACTOR_STATE_RUNNING,
			WorkerAssignment: &ateapipb.WorkerAssignment{
				Worker:          &ateapipb.ObjectRef{Name: workerName},
				WorkerNamespace: "default",
				WorkerPool:      "pool1",
				WorkerPod:       "worker-pod-1",
				WorkerPodUid:    workerName,
				NodeName:        "node-1",
				SandboxRuntime:  wantRuntime,
			},
		},
	})
	w, atelet := newWireCaptureWorkflow(t, persistence)
	var marked *ateapipb.Actor
	atelet.onCheckpoint = func(ctx context.Context, _ *ateletpb.CheckpointRequest) error {
		var err error
		marked, err = persistence.GetActor(ctx, actorRef)
		return err
	}
	if _, err := w.SuspendActor(ctx, actorRef); err != nil {
		t.Fatalf("SuspendActor: %v", err)
	}

	inProgressSnap := findLatestDurableSnapshot(marked.GetStatus(), ateapipb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_PENDING)
	uri := mustParseSnapshotURI(t, inProgressSnap.GetDurableSnapshot().GetObject().GetSnapshotUri())
	if !resources.IsValidResourceName(uri.Name()) {
		t.Errorf("in-progress snapshot name = %q, want a valid resource name", uri.Name())
	}
	if got := inProgressSnap.GetUuid(); got != uri.Name() {
		t.Errorf("Snapshot.Uuid = %q, want the snapshot name in its URI, %q", got, uri.Name())
	}
	want := "gs://bucket/root/atespaces/team-a/actors/" + marked.GetMetadata().GetUid() + "/snapshots/" + uri.Name()
	if uri.String() != want {
		t.Errorf("snapshot URI = %q, want %q", uri, want)
	}
	if diff := cmp.Diff(wantRuntime, inProgressSnap.GetSandboxRuntime(), protocmp.Transform()); diff != "" {
		t.Errorf("Snapshot.SandboxRuntime mismatch (-want +got):\n%s", diff)
	}
}

// TestEnsureMarkedSuspending_ReentryKeepsPersistedSnapshotLocation verifies a
// re-entered workflow does not mint a second snapshot location: the location
// persisted by the first attempt stays authoritative.
func TestEnsureMarkedSuspending_ReentryKeepsPersistedSnapshotLocation(t *testing.T) {
	const firstAttempt = "gs://bucket/root/atespaces/team-a/actors/actor-uid/snapshots/first-attempt"

	ctx := context.Background()
	persistence := newTestPersistence(t)
	seedSubstrateTemplate(t, ctx, persistence, "sub-tmpl")
	actorRef := resources.ActorRef{Atespace: "team-a", Name: "actor-1"}
	storetest.MustCreateActor(t, ctx, persistence, &ateapipb.Actor{
		Metadata:      &ateapipb.ResourceMetadata{Atespace: actorRef.Atespace, Name: actorRef.Name},
		ActorTemplate: &ateapipb.ObjectRef{Atespace: "team-a", Name: "sub-tmpl"},
		Status: &ateapipb.ActorStatus{
			State:                  ateapipb.ActorState_ACTOR_STATE_SUSPENDING,
			WorkerAssignment:       wireTestAssignment(),
			LastAssignedGeneration: 1,
			Snapshots: []*ateapipb.Snapshot{
				newDurableSnapshot(1, ateapipb.SnapshotOwner_SNAPSHOT_OWNER_ACTOR, ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY, "", "first-attempt", firstAttempt, ateapipb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_PENDING),
			},
		},
	})
	w, atelet := newWireCaptureWorkflow(t, persistence)
	suspended, err := w.SuspendActor(ctx, actorRef)
	if err != nil {
		t.Fatalf("SuspendActor: %v", err)
	}
	if got := atelet.checkpointRequest().GetSnapshot().GetObject().GetSnapshotUri(); got != firstAttempt {
		t.Errorf("CheckpointRequest SnapshotUri = %q, want the first attempt's location %q", got, firstAttempt)
	}
	if gotSnap := findLatestDurableSnapshot(suspended.GetStatus(), ateapipb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_COMPLETED); gotSnap.GetDurableSnapshot().GetObject().GetSnapshotUri() != firstAttempt {
		t.Errorf("completed durable snapshot URI = %q, want the first attempt's location", gotSnap.GetDurableSnapshot().GetObject().GetSnapshotUri())
	}
}

// TestSuspendActorWorkflow_RejectedAndIdempotentPaths covers the two
// short-circuit paths of the suspend workflow: rejection of the suspend edge
// for a non-RUNNING actor and the idempotent fast-forward for a SUSPENDED one.
func TestSuspendActorWorkflow_RejectedAndIdempotentPaths(t *testing.T) {
	tests := []struct {
		name      string
		seedState ateapipb.ActorState
		// wantErr true means SuspendActor must fail with FailedPrecondition.
		wantErr bool
		// wantState is the stored state after the call.
		wantState ateapipb.ActorState
	}{
		{
			// Suspending a SUSPENDED actor succeeds idempotently via
			// IsComplete fast-forward without calling atelet.
			name:      "newly created suspended succeeds",
			seedState: ateapipb.ActorState_ACTOR_STATE_SUSPENDED,
			wantState: ateapipb.ActorState_ACTOR_STATE_SUSPENDED,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st, cleanup := storetest.SetupTestStore(t)
			defer cleanup()
			w := newTestActorWorkflow(t, st, "ns", "tmpl1")

			seedWorkflowActor(t, ctx, st, resources.ActorRef{Atespace: "team-a", Name: "id1"}, "ns", "tmpl1", tc.seedState)

			actor, err := w.SuspendActor(ctx, resources.ActorRef{Atespace: "team-a", Name: "id1"})
			if tc.wantErr {
				if got := apierror.Code(err); got != codes.FailedPrecondition {
					t.Fatalf("apierror.Code(err) = %v, want %v (err: %v)", got, codes.FailedPrecondition, err)
				}
			} else {
				if err != nil {
					t.Fatalf("SuspendActor failed: %v", err)
				}
				if actor.GetStatus().GetState() != tc.wantState {
					t.Errorf("returned state = %v, want %v", actor.GetStatus().GetState(), tc.wantState)
				}
			}

			got, err := st.GetActor(ctx, resources.ActorRef{Atespace: "team-a", Name: "id1"})
			if err != nil {
				t.Fatalf("GetActor failed: %v", err)
			}
			if got.GetStatus().GetState() != tc.wantState {
				t.Errorf("stored state = %v, want %v", got.GetStatus().GetState(), tc.wantState)
			}
		})
	}
}

// TestEnsureMarkedSuspending_StateMatrix verifies the suspend edge's state
// gating against every actor state: RUNNING takes the edge (checkpoint the
// workload), SUSPENDING skips (a previous attempt already marked the actor),
// everything else is rejected with FailedPrecondition. SUSPENDED without a
// node-local snapshot is rejected here because the orchestrator
// early-returns before this step for a fully suspended actor; SUSPENDED with
// one is the paused shape, covered by
// TestEnsureMarkedSuspending_PausedPromotesLocalSnapshot.
func TestEnsureMarkedSuspending_StateMatrix(t *testing.T) {
	allowed := map[ateapipb.ActorState]bool{
		ateapipb.ActorState_ACTOR_STATE_RUNNING:    true,
		ateapipb.ActorState_ACTOR_STATE_SUSPENDING: true, // skipped, not re-marked
		ateapipb.ActorState_ACTOR_STATE_SUSPENDED:  true, // idempotent fast-forward
	}

	for _, seedState := range allActorStates {
		ctx := context.Background()
		persistence := newTestPersistence(t)
		seedSubstrateTemplate(t, ctx, persistence, "sub-tmpl")
		w, atelet := newWireCaptureWorkflow(t, persistence)

		actorRef := resources.ActorRef{Atespace: "team-a", Name: "id1"}
		status := &ateapipb.ActorStatus{State: seedState}
		switch seedState {
		case ateapipb.ActorState_ACTOR_STATE_RUNNING:
			status.WorkerAssignment = wireTestAssignment()
		case ateapipb.ActorState_ACTOR_STATE_SUSPENDING:
			status.WorkerAssignment = wireTestAssignment()
			status.LastAssignedGeneration = 1
			status.Snapshots = []*ateapipb.Snapshot{
				newDurableSnapshot(1, ateapipb.SnapshotOwner_SNAPSHOT_OWNER_ACTOR, ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY, "", "snap-1", someActorSnapshotURI(t, testStorageLocation, "team-a", "snap-1"), ateapipb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_PENDING),
			}
		}
		storetest.MustCreateActor(t, ctx, persistence, &ateapipb.Actor{
			Metadata:      &ateapipb.ResourceMetadata{Atespace: actorRef.Atespace, Name: actorRef.Name},
			ActorTemplate: &ateapipb.ObjectRef{Atespace: "team-a", Name: "sub-tmpl"},
			Status:        status,
		})

		atelet.onCheckpoint = func(ctx context.Context, _ *ateletpb.CheckpointRequest) error {
			marked, err := persistence.GetActor(ctx, actorRef)
			if err != nil {
				t.Fatalf("GetActor during checkpoint: %v", err)
			}
			if marked.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_SUSPENDING {
				t.Errorf("state %v: MarkSuspending stored actor in %v, want SUSPENDING", seedState, marked.GetStatus().GetState())
			}
			return nil
		}

		suspended, err := w.SuspendActor(ctx, actorRef)
		assertPrerequisiteResult(t, seedState, err, allowed[seedState])
		if err == nil && suspended.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_SUSPENDED {
			t.Errorf("state %v: SuspendActor returned actor in %v, want SUSPENDED", seedState, suspended.GetStatus().GetState())
		}
	}
}

// TestEnsureMarkedSuspending_RejectsPauseInProgress verifies a suspend does
// not adopt a SUSPENDING actor whose latest generation is a pending
// node-local snapshot: that actor is mid-pause, and the pause owns it until
// it is paused.
func TestEnsureMarkedSuspending_RejectsPauseInProgress(t *testing.T) {
	ctx := context.Background()
	persistence := newTestPersistence(t)
	seedSubstrateTemplate(t, ctx, persistence, "sub-tmpl")
	w := &ActorWorkflow{store: persistence}
	actorRef := resources.ActorRef{Atespace: "team-a", Name: "id1"}

	pausing := &ateapipb.Actor{
		Metadata:      &ateapipb.ResourceMetadata{Atespace: actorRef.Atespace, Name: actorRef.Name},
		ActorTemplate: &ateapipb.ObjectRef{Atespace: "team-a", Name: "sub-tmpl"},
		Status:        &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_SUSPENDING},
	}
	withPauseInProgress()(pausing)
	storetest.MustCreateActor(t, ctx, persistence, pausing)

	_, err := w.SuspendActor(ctx, actorRef)
	if got := apierror.Code(err); got != codes.FailedPrecondition {
		t.Errorf("SuspendActor on a pause in progress: code = %v, want FailedPrecondition (err: %v)", got, err)
	}
	stored, err := persistence.GetActor(ctx, actorRef)
	if err != nil {
		t.Fatalf("GetActor: %v", err)
	}
	if got := stored.GetStatus().GetLastAssignedGeneration(); got != 1 {
		t.Errorf("last_assigned_generation = %d, want 1 (the pause's snapshot untouched)", got)
	}
}

// TestEnsureMarkedSuspending_PausedPromotesLocalSnapshot verifies the suspend
// edge from a paused actor (SUSPENDED whose latest snapshot is node-local):
// it takes the edge and promotes that snapshot in place to a pending durable
// one, keeping its generation, uuid, and sandbox runtime stamp, with the
// upload destination derived from the uuid.
func TestEnsureMarkedSuspending_PausedPromotesLocalSnapshot(t *testing.T) {
	ctx := context.Background()
	persistence := newTestPersistence(t)
	if _, err := persistence.CreateActorTemplate(ctx, &ateapipb.ActorTemplate{
		Metadata:       &ateapipb.ResourceMetadata{Atespace: "team-a", Name: "sub-tmpl"},
		SnapshotConfig: &ateapipb.SnapshotConfig{StorageLocation: "gs://snapshots"},
		SandboxConfig:  &ateapipb.SandboxConfig{SandboxClass: "gvisor", ConfigName: "gvisor"},
	}); err != nil {
		t.Fatalf("CreateActorTemplate: %v", err)
	}
	w, atelet := newWireCaptureWorkflow(t, persistence)

	actorRef := resources.ActorRef{Atespace: "team-a", Name: "id1"}
	localSnap := newLocalSnapshot(1, ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY, "", "snap-1", "node-1")
	localSnap.SandboxRuntime = testSandboxRuntime()
	actor := storetest.MustCreateActor(t, ctx, persistence, &ateapipb.Actor{
		Metadata:      &ateapipb.ResourceMetadata{Atespace: actorRef.Atespace, Name: actorRef.Name},
		ActorTemplate: &ateapipb.ObjectRef{Atespace: "team-a", Name: "sub-tmpl"},
		Status: &ateapipb.ActorStatus{
			State:                  ateapipb.ActorState_ACTOR_STATE_SUSPENDED,
			AssignedNode:           "node-1",
			LastAssignedGeneration: 1,
			Snapshots:              []*ateapipb.Snapshot{localSnap},
		},
	})

	var marked *ateapipb.Actor
	atelet.onPromote = func(ctx context.Context, _ *ateletpb.PromoteSnapshotRequest) error {
		var err error
		marked, err = persistence.GetActor(ctx, actorRef)
		return err
	}
	if _, err := w.SuspendActor(ctx, actorRef); err != nil {
		t.Fatalf("SuspendActor: %v", err)
	}
	if got := marked.GetStatus().GetState(); got != ateapipb.ActorState_ACTOR_STATE_SUSPENDING {
		t.Errorf("state = %v, want SUSPENDING", got)
	}
	wantURI, err := resources.NewActorSnapshotURI("gs://snapshots", actorRef.Atespace, actor.GetMetadata().GetUid(), "snap-1")
	if err != nil {
		t.Fatalf("NewActorSnapshotURI: %v", err)
	}
	pending := findLatestDurableSnapshot(marked.GetStatus(), ateapipb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_PENDING)
	gotURI := pending.GetDurableSnapshot().GetObject().GetSnapshotUri()
	if gotURI != wantURI.String() {
		t.Errorf("paused-origin in-progress SnapshotUri = %q, want %q (derived from local snapshot uuid)", gotURI, wantURI.String())
	}
	// The upload promotes the pause snapshot in place rather than minting a
	// new generation.
	if pending.GetGeneration() != 1 || pending.GetUuid() != "snap-1" {
		t.Errorf("paused-origin pending snapshot = generation %d, uuid %q; want the pause snapshot (generation 1, uuid %q)", pending.GetGeneration(), pending.GetUuid(), "snap-1")
	}
	if pending.GetLocality() != "node-1" {
		t.Errorf("pending snapshot locality = %q, want node-1 kept until the upload finishes", pending.GetLocality())
	}
	// The durable copy is the pause checkpoint uploaded, so it keeps the
	// runtime stamp the pause recorded.
	if diff := cmp.Diff(testSandboxRuntime(), pending.GetSandboxRuntime(), protocmp.Transform()); diff != "" {
		t.Errorf("paused-origin pending Snapshot.SandboxRuntime mismatch (-want +got):\n%s", diff)
	}
	if got := marked.GetStatus().GetLastAssignedGeneration(); got != 1 {
		t.Errorf("paused-origin LastAssignedGeneration = %d, want 1", got)
	}
}

// TestSuspendActor_CrashesWhenSuspendingActorMissingWorkerPod verifies that a
// SUSPENDING actor with no worker pod recorded is moved to CRASHED by
// CallAteletSuspendStep's prerequisite check and the suspend fails with
// FailedPrecondition.
func TestSuspendActor_CrashesWhenSuspendingActorMissingWorkerPod(t *testing.T) {
	ctx := context.Background()
	st, cleanup := storetest.SetupTestStore(t)
	defer cleanup()
	w := newTestActorWorkflow(t, st, "ns", "tmpl1")

	seedWorkflowActor(t, ctx, st, resources.ActorRef{Atespace: "team-a", Name: "id1"}, "ns", "tmpl1", ateapipb.ActorState_ACTOR_STATE_SUSPENDING)

	_, err := w.SuspendActor(ctx, resources.ActorRef{Atespace: "team-a", Name: "id1"})
	if got := apierror.Code(err); got != codes.FailedPrecondition {
		t.Fatalf("apierror.Code(err) = %v, want %v (err: %v)", got, codes.FailedPrecondition, err)
	}

	got, err := st.GetActor(ctx, resources.ActorRef{Atespace: "team-a", Name: "id1"})
	if err != nil {
		t.Fatalf("GetActor failed: %v", err)
	}
	if got.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_CRASHED {
		t.Errorf("stored state = %v, want %v", got.GetStatus().GetState(), ateapipb.ActorState_ACTOR_STATE_CRASHED)
	}
	if msg, want := got.GetStatus().GetCrash().GetMessage(), "suspend failed: "+crashMessageWorkerAssignmentMissing; msg != want {
		t.Errorf("crash message = %q, want %q", msg, want)
	}
}

// newTestPersistence returns an isolated PostgreSQL-backed store.
func newTestPersistence(t *testing.T) store.Interface {
	persistence, _ := storetest.SetupTestStore(t)
	storetest.MustCreateAtespace(t, context.Background(), persistence, "team-a")
	return persistence
}

// newDanglingDialer returns a dialer whose informer cache has no pods, so
// DialForAteletOnNode always returns ErrNoAteletOnNode.
func newDanglingDialer() *AteletDialer {
	empty := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{
		byNode: func(obj any) ([]string, error) { return nil, nil },
	})
	return NewAteletDialer(empty, installdefaults.AteletSPIFFEID(installdefaults.SystemNamespace), "", "")
}

func TestEnsureAteletSuspended_DialFailureLeavesActorRetryable(t *testing.T) {
	neverWritten := someActorSnapshotURI(t, testStorageLocation, "team-a", "never-written")

	tests := []struct {
		name         string
		prevSnapshot string
	}{
		{
			name:         "keeps previous external snapshot",
			prevSnapshot: someActorSnapshotURI(t, testStorageLocation, "team-a", "prev"),
		},
		{
			name:         "stays empty without previous external snapshot",
			prevSnapshot: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			persistence := newTestPersistence(t)
			seedSubstrateTemplate(t, ctx, persistence, "sub-tmpl")
			var snapshots []*ateapipb.Snapshot
			if tt.prevSnapshot != "" {
				snapshots = append(snapshots, newDurableSnapshot(1, ateapipb.SnapshotOwner_SNAPSHOT_OWNER_ACTOR, ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY, "", "prev-snapshot", tt.prevSnapshot, ateapipb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_COMPLETED))
			}
			snapshots = append(snapshots, newDurableSnapshot(2, ateapipb.SnapshotOwner_SNAPSHOT_OWNER_ACTOR, ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY, "", "never-written", neverWritten, ateapipb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_PENDING))

			actor := &ateapipb.Actor{
				Metadata:      &ateapipb.ResourceMetadata{Atespace: "team-a", Name: "actor-1"},
				ActorTemplate: &ateapipb.ObjectRef{Atespace: "team-a", Name: "sub-tmpl"},
				Status: &ateapipb.ActorStatus{
					State: ateapipb.ActorState_ACTOR_STATE_SUSPENDING,
					WorkerAssignment: &ateapipb.WorkerAssignment{
						WorkerNamespace: "worker-ns",
						WorkerPool:      "pool",
						WorkerPod:       "pod-gone",
						NodeName:        "node-gone",
					},
					LastAssignedGeneration: 2,
					Snapshots:              snapshots,
				},
			}
			storetest.MustCreateActor(t, ctx, persistence, actor)

			w := &ActorWorkflow{store: persistence, dialer: newDanglingDialer()}
			if _, err := w.SuspendActor(ctx, resources.ActorRef{Atespace: "team-a", Name: "actor-1"}); err == nil {
				t.Fatal("SuspendActor: want error when atelet is unreachable, got nil")
			}

			// A dial failure is transient from this workflow's point of view: it
			// must not crash the actor or touch its snapshot state. A worker that
			// is genuinely gone is handled by the DeleteWorker workflow instead.
			stored, err := persistence.GetActor(ctx, resources.ActorRef{Atespace: "team-a", Name: "actor-1"})
			if err != nil {
				t.Fatalf("GetActor: %v", err)
			}
			if stored.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_SUSPENDING {
				t.Errorf("state = %v, want unchanged SUSPENDING", stored.GetStatus().GetState())
			}
			if gotSnap := findLatestDurableSnapshot(stored.GetStatus(), ateapipb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_PENDING); gotSnap.GetDurableSnapshot().GetObject().GetSnapshotUri() != neverWritten {
				t.Errorf("in-progress durable snapshot URI = %q, want preserved for debugging", gotSnap.GetDurableSnapshot().GetObject().GetSnapshotUri())
			}
			gotSnap := findLatestDurableSnapshot(stored.GetStatus(), ateapipb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_COMPLETED)
			if got := gotSnap.GetDurableSnapshot().GetObject().GetSnapshotUri(); got != tt.prevSnapshot {
				t.Errorf("SnapshotUri = %q, want %q", got, tt.prevSnapshot)
			}
		})
	}
}

// TestEnsureAteletSuspended_WireRequest verifies a running-origin suspend
// checkpoints to the actor's in-progress durable snapshot with an immediate
// upload, since FinalizeSuspended then marks that snapshot COMPLETED.
func TestEnsureAteletSuspended_WireRequest(t *testing.T) {
	ctx := context.Background()
	persistence := newTestPersistence(t)
	w, atelet := newWireCaptureWorkflow(t, persistence)

	storetest.MustCreateAtespace(t, ctx, persistence, "ns")
	if _, err := persistence.CreateActorTemplate(ctx, &ateapipb.ActorTemplate{
		Metadata:       &ateapipb.ResourceMetadata{Atespace: "ns", Name: "tmpl1"},
		SnapshotConfig: &ateapipb.SnapshotConfig{StorageLocation: testStorageLocation, PreferredFidelity: ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY},
		SandboxConfig:  &ateapipb.SandboxConfig{SandboxClass: "gvisor", ConfigName: "gvisor"},
	}); err != nil {
		t.Fatalf("CreateActorTemplate: %v", err)
	}

	actorRef := resources.ActorRef{Atespace: "team-a", Name: "actor-1"}
	snapshotName := "2026-01-01t00-00-00z-abc"
	snapshotURI := someActorSnapshotURI(t, testStorageLocation, "team-a", snapshotName)
	storetest.MustCreateActor(t, ctx, persistence, &ateapipb.Actor{
		Metadata:      &ateapipb.ResourceMetadata{Atespace: actorRef.Atespace, Name: actorRef.Name},
		ActorTemplate: &ateapipb.ObjectRef{Atespace: "ns", Name: "tmpl1"},
		Status: &ateapipb.ActorStatus{
			State:                  ateapipb.ActorState_ACTOR_STATE_SUSPENDING,
			AssignedNode:           "node-1",
			WorkerAssignment:       wireTestAssignment(),
			LastAssignedGeneration: 1,
			Snapshots: []*ateapipb.Snapshot{
				newDurableSnapshot(1, ateapipb.SnapshotOwner_SNAPSHOT_OWNER_ACTOR, ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY, "tmpl-uid-1", snapshotName, snapshotURI, ateapipb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_PENDING),
			},
		},
	})

	if _, err := w.SuspendActor(ctx, actorRef); err != nil {
		t.Fatalf("SuspendActor: %v", err)
	}
	req := atelet.checkpointRequest()
	if req == nil {
		t.Fatal("expected Checkpoint RPC to be called")
	}
	if got := req.GetSnapshot().GetSnapshotUuid(); got != snapshotName {
		t.Errorf("CheckpointRequest.Snapshot.SnapshotUuid = %q, want %q", got, snapshotName)
	}
	if got := req.GetSnapshot().GetObject().GetSnapshotUri(); got != snapshotURI {
		t.Errorf("CheckpointRequest.Snapshot.Object.SnapshotUri = %q, want %q", got, snapshotURI)
	}
	wantConfig := &ateletpb.UploadConfig{Mode: ateletpb.UploadMode_UPLOAD_MODE_IMMEDIATE}
	if got := req.GetUploadConfig(); !proto.Equal(got, wantConfig) {
		t.Errorf("CheckpointRequest.UploadConfig = %v, want %v", got, wantConfig)
	}
}

// TestEnsureSuspendedFinalized_NoAssignment verifies finalization runs even when
// the actor has no worker assignment: the external snapshot must be recorded on
// the actor and the actor moved to SUSPENDED rather than silently left
// SUSPENDING. This is the shape a paused-origin suspend (#791) produces — a
// paused actor has no worker — and the regression test for finalization
// previously living inside the worker-freeing branch.
func TestEnsureSuspendedFinalized_NoAssignment(t *testing.T) {
	ctx := context.Background()
	persistence := newTestPersistence(t)
	seedSubstrateTemplate(t, ctx, persistence, "sub-tmpl")

	snapshotURI := someActorSnapshotURI(t, testStorageLocation, "team-a", "2026-01-01t00-00-00z-abc")
	snap := newDurableSnapshot(1, ateapipb.SnapshotOwner_SNAPSHOT_OWNER_ACTOR, ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY, "tmpl-uid-1", "actor-1-pause-snapshot", snapshotURI, ateapipb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_PENDING)
	snap.Locality = "node-1"
	actor := &ateapipb.Actor{
		Metadata:      &ateapipb.ResourceMetadata{Atespace: "team-a", Name: "actor-1"},
		ActorTemplate: &ateapipb.ObjectRef{Atespace: "team-a", Name: "sub-tmpl"},
		Status: &ateapipb.ActorStatus{
			State:                  ateapipb.ActorState_ACTOR_STATE_SUSPENDING,
			AssignedNode:           "node-1",
			LastAssignedGeneration: 1,
			Snapshots:              []*ateapipb.Snapshot{snap},
		},
	}
	storetest.MustCreateActor(t, ctx, persistence, actor)

	w, _ := newWireCaptureWorkflow(t, persistence)
	stored, err := w.SuspendActor(ctx, resources.ActorRef{Atespace: "team-a", Name: "actor-1"})
	if err != nil {
		t.Fatalf("SuspendActor: %v", err)
	}

	if stored.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_SUSPENDED {
		t.Errorf("state = %v, want SUSPENDED", stored.GetStatus().GetState())
	}
	gotSnap := findLatestDurableSnapshot(stored.GetStatus(), ateapipb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_COMPLETED)
	if got := gotSnap.GetDurableSnapshot().GetObject().GetSnapshotUri(); got != snapshotURI {
		t.Errorf("SnapshotUri = %q, want %q", got, snapshotURI)
	}
	// The snapshot carries the template it was captured under, so a later
	// repoint can tell that its guest state no longer matches.
	if got := gotSnap.GetActorTemplateUid(); got != "tmpl-uid-1" {
		t.Errorf("Snapshot.ActorTemplateUid = %q, want %q", got, "tmpl-uid-1")
	}
	if got := findLatestDurableSnapshot(stored.GetStatus(), ateapipb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_PENDING); got != nil {
		t.Errorf("in-progress durable snapshot = %v, want cleared", got)
	}
	if local := findLatestLocalSnapshot(stored.GetStatus()); local != nil {
		t.Errorf("LocalSnapshot = %v, want cleared", local)
	}
	if got := stored.GetStatus().GetAssignedNode(); got != "" {
		t.Errorf("AssignedNode = %q, want cleared", got)
	}
}

// TestEnsureSuspendedFinalized_ReleasesReplacedSnapshot verifies which external
// snapshot a suspend collects. The actor's previous one goes away, because
// nothing else can name it — unless the actor was only borrowing it from a tag,
// in which case the tag owns those objects and the suspend must leave them be.
func TestEnsureSuspendedFinalized_ReleasesReplacedSnapshot(t *testing.T) {
	// The name of the snapshot the suspend has just written, in place of the
	// one the actor was running from.
	const snapshotName = "2026-01-01t00-00-00z-new"

	tests := []struct {
		name string
		// tagOwnedSnapshot makes the snapshot the actor is running from a tag's rather
		// than one it took itself, which is how an actor created from a tag
		// starts out.
		tagOwnedSnapshot bool
		wantReleased     bool
	}{
		{
			name:         "releases the external snapshot the actor owned",
			wantReleased: true,
		},
		{
			name:             "leaves an external snapshot borrowed from a tag in place",
			tagOwnedSnapshot: true,
			wantReleased:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			persistence := newTestPersistence(t)
			template := seedSubstrateTemplate(t, ctx, persistence, "sub-tmpl")
			objects := objectstoretest.New()
			w, _ := newWireCaptureWorkflow(t, persistence)
			w.objectStorePlugin = objectstoreplugintest.ControlClient(objects)

			actorRef := resources.ActorRef{Atespace: "team-a", Name: "actor-1"}
			actor := storetest.MustCreateActor(t, ctx, persistence, &ateapipb.Actor{
				Metadata:      &ateapipb.ResourceMetadata{Atespace: actorRef.Atespace, Name: actorRef.Name},
				ActorTemplate: &ateapipb.ObjectRef{Atespace: "team-a", Name: "sub-tmpl"},
				Status:        &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_SUSPENDING},
			})

			previous := mustActorSnapshotURI(t, template, actor, "old")
			prevType := ateapipb.SnapshotOwner_SNAPSHOT_OWNER_ACTOR
			if tt.tagOwnedSnapshot {
				previous = mustTagSnapshotURI(t, template, "team-a", "v1-snapshot")
				prevType = ateapipb.SnapshotOwner_SNAPSHOT_OWNER_TAG
			}
			fresh := mustActorSnapshotURI(t, template, actor, snapshotName)
			objects.PutSnapshot(t, previous, "manifest.json")
			objects.PutSnapshot(t, fresh, "manifest.json")

			mustUpdateActorStatus(t, ctx, persistence, actor, func(s *ateapipb.ActorStatus) {
				s.WorkerAssignment = wireTestAssignment()
				s.LastAssignedGeneration = 2
				s.Snapshots = []*ateapipb.Snapshot{
					newDurableSnapshot(1, prevType, ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY, "", previous.Name(), previous.String(), ateapipb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_COMPLETED),
					newDurableSnapshot(2, ateapipb.SnapshotOwner_SNAPSHOT_OWNER_ACTOR, ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY, "", fresh.Name(), fresh.String(), ateapipb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_PENDING),
				}
			})

			stored, err := w.SuspendActor(ctx, actorRef)
			if err != nil {
				t.Fatalf("SuspendActor: %v", err)
			}
			// Whichever way the previous snapshot went, the actor now owns the
			// one it just wrote and is no longer borrowing.
			gotSnap := findLatestDurableSnapshot(stored.GetStatus(), ateapipb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_COMPLETED)
			if got := gotSnap.GetDurableSnapshot().GetObject().GetSnapshotUri(); got != fresh.String() {
				t.Errorf("external snapshot = %q, want the one this suspend wrote, %q", got, fresh)
			}
			if released := len(objects.Snapshot(t, previous)) == 0; released != tt.wantReleased {
				t.Errorf("previous external snapshot released = %v, want %v", released, tt.wantReleased)
			}
			if len(objects.Snapshot(t, fresh)) == 0 {
				t.Error("the external snapshot this suspend wrote was collected")
			}
		})
	}
}

// errObjectStore stands in for object storage being unreachable.
var errObjectStore = errors.New("object storage is unavailable")

// isObjectStoreErr reports whether err came from errObjectStore. The object
// store sits behind the object-store plugin's gRPC boundary, which carries the
// message but not the error value, so errors.Is cannot match it.
func isObjectStoreErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), errObjectStore.Error())
}

// TestEnsureSuspendedFinalized_CommitsDespiteObjectStoreFailure verifies that
// failing to collect the snapshot a suspend replaced does not fail the
// suspend. The worker is already released by then, so aborting would leave the
// actor SUSPENDING with no way to resume; it commits SUSPENDED on the new
// snapshot instead and leaves the replaced one in storage.
func TestEnsureSuspendedFinalized_CommitsDespiteObjectStoreFailure(t *testing.T) {
	ctx := context.Background()
	persistence := newTestPersistence(t)
	template := seedSubstrateTemplate(t, ctx, persistence, "sub-tmpl")
	objects := objectstoretest.New()
	w, _ := newWireCaptureWorkflow(t, persistence)
	w.objectStorePlugin = objectstoreplugintest.ControlClient(objects)

	const snapshotName = "2026-01-01t00-00-00z-new"
	actorRef := resources.ActorRef{Atespace: "team-a", Name: "actor-1"}
	actor := storetest.MustCreateActor(t, ctx, persistence, &ateapipb.Actor{
		Metadata:      &ateapipb.ResourceMetadata{Atespace: actorRef.Atespace, Name: actorRef.Name},
		ActorTemplate: &ateapipb.ObjectRef{Atespace: "team-a", Name: "sub-tmpl"},
		Status:        &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_SUSPENDING},
	})

	previous := mustActorSnapshotURI(t, template, actor, "old")
	fresh := mustActorSnapshotURI(t, template, actor, snapshotName)
	objects.PutSnapshot(t, previous, "manifest.json")
	objects.PutSnapshot(t, fresh, "manifest.json")

	mustUpdateActorStatus(t, ctx, persistence, actor, func(s *ateapipb.ActorStatus) {
		s.WorkerAssignment = wireTestAssignment()
		s.LastAssignedGeneration = 2
		s.Snapshots = []*ateapipb.Snapshot{
			newDurableSnapshot(1, ateapipb.SnapshotOwner_SNAPSHOT_OWNER_ACTOR, ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY, "", previous.Name(), previous.String(), ateapipb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_COMPLETED),
			newDurableSnapshot(2, ateapipb.SnapshotOwner_SNAPSHOT_OWNER_ACTOR, ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY, "", fresh.Name(), fresh.String(), ateapipb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_PENDING),
		}
	})

	objects.OnDelete = func(string, string) error { return errObjectStore }
	stored, err := w.SuspendActor(ctx, actorRef)
	if err != nil {
		t.Fatalf("SuspendActor: %v", err)
	}
	if got := stored.GetStatus().GetState(); got != ateapipb.ActorState_ACTOR_STATE_SUSPENDED {
		t.Errorf("state = %v, want SUSPENDED", got)
	}
	gotSnap := findLatestDurableSnapshot(stored.GetStatus(), ateapipb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_COMPLETED)
	if got := gotSnap.GetDurableSnapshot().GetObject().GetSnapshotUri(); got != fresh.String() {
		t.Errorf("actor snapshot uri = %q, want the one this suspend wrote, %q", got, fresh)
	}
	if got := findLatestDurableSnapshot(stored.GetStatus(), ateapipb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_PENDING); got != nil {
		t.Errorf("in-progress durable snapshot = %v, want cleared", got)
	}
	if len(objects.Snapshot(t, previous)) == 0 {
		t.Error("replaced external snapshot was collected despite every delete failing")
	}
	if len(objects.Snapshot(t, fresh)) == 0 {
		t.Error("the external snapshot this suspend wrote was collected")
	}
}

// TestEnsureSuspendedFinalized_KeepsReplacedSnapshotOnConflict verifies that a
// commit lost to a concurrent update releases nothing: the actor record still
// names its old snapshot, so collecting it would leave the actor pointing at
// deleted objects.
func TestEnsureSuspendedFinalized_KeepsReplacedSnapshotOnConflict(t *testing.T) {
	ctx := context.Background()
	persistence := newTestPersistence(t)
	template := seedSubstrateTemplate(t, ctx, persistence, "sub-tmpl")
	objects := objectstoretest.New()

	actorRef := resources.ActorRef{Atespace: "team-a", Name: "actor-1"}
	actor := storetest.MustCreateActor(t, ctx, persistence, &ateapipb.Actor{
		Metadata:      &ateapipb.ResourceMetadata{Atespace: actorRef.Atespace, Name: actorRef.Name},
		ActorTemplate: &ateapipb.ObjectRef{Atespace: "team-a", Name: "sub-tmpl"},
		Status:        &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_SUSPENDING},
	})

	previous := mustActorSnapshotURI(t, template, actor, "old")
	fresh := mustActorSnapshotURI(t, template, actor, "2026-01-01t00-00-00z-new")
	objects.PutSnapshot(t, previous, "manifest.json")
	objects.PutSnapshot(t, fresh, "manifest.json")
	mustUpdateActorStatus(t, ctx, persistence, actor, func(s *ateapipb.ActorStatus) {
		s.WorkerAssignment = wireTestAssignment()
		s.LastAssignedGeneration = 2
		s.Snapshots = []*ateapipb.Snapshot{
			newDurableSnapshot(1, ateapipb.SnapshotOwner_SNAPSHOT_OWNER_ACTOR, ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY, "", previous.Name(), previous.String(), ateapipb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_COMPLETED),
			newDurableSnapshot(2, ateapipb.SnapshotOwner_SNAPSHOT_OWNER_ACTOR, ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY, "", fresh.Name(), fresh.String(), ateapipb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_PENDING),
		}
	})

	w, _ := newWireCaptureWorkflow(t, &conflictingUpdateStore{Interface: persistence})
	w.objectStorePlugin = objectstoreplugintest.ControlClient(objects)
	if _, err := w.SuspendActor(ctx, actorRef); apierror.Code(err) != codes.Aborted {
		t.Fatalf("SuspendActor = %v, want code Aborted", err)
	}
	if len(objects.Snapshot(t, previous)) == 0 {
		t.Error("replaced external snapshot was collected though the commit lost")
	}
}

// conflictingUpdateStore fails every actor update with a version conflict, as
// if another writer always got there first.
type conflictingUpdateStore struct {
	store.Interface
}

func (conflictingUpdateStore) UpdateActor(context.Context, resources.ActorRef, store.Precondition, func(*ateapipb.Actor) error) (*ateapipb.Actor, error) {
	return nil, store.ErrVersionConflict
}

// TestEnsureSuspendedFinalized_KeepsCrashFromWorkerDelete verifies that a
// crash the DeleteWorker workflow commits while ensureSuspendedFinalized is
// freeing the worker is kept: the step's update is pinned to the actor version
// it checked, so it fails Aborted instead of committing SUSPENDED over the
// crash.
func TestEnsureSuspendedFinalized_KeepsCrashFromWorkerDelete(t *testing.T) {
	ctx := context.Background()
	persistence := newTestPersistence(t)
	seedSubstrateTemplate(t, ctx, persistence, "sub-tmpl")
	actorRef := resources.ActorRef{Atespace: "team-a", Name: "actor-1"}

	workerName := testWorkerUID("worker-pod-1")
	created := storetest.MustCreateActor(t, ctx, persistence, &ateapipb.Actor{
		Metadata:      &ateapipb.ResourceMetadata{Atespace: actorRef.Atespace, Name: actorRef.Name},
		ActorTemplate: &ateapipb.ObjectRef{Atespace: "team-a", Name: "sub-tmpl"},
		Status: &ateapipb.ActorStatus{
			State:        ateapipb.ActorState_ACTOR_STATE_SUSPENDING,
			AssignedNode: "node1",
			WorkerAssignment: &ateapipb.WorkerAssignment{
				Worker:          &ateapipb.ObjectRef{Name: workerName},
				WorkerNamespace: "default",
				WorkerPool:      "pool1",
				WorkerPod:       "worker-pod-1",
				WorkerPodUid:    workerName,
				NodeName:        "node-1",
			},
			LastAssignedGeneration: 1,
			Snapshots: []*ateapipb.Snapshot{
				newDurableSnapshot(1, ateapipb.SnapshotOwner_SNAPSHOT_OWNER_ACTOR, ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY, "", "snapshot-1", someActorSnapshotURI(t, testStorageLocation, "team-a", "snapshot-1"), ateapipb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_PENDING),
			},
		},
	})
	if _, err := persistence.CreateWorker(ctx, &ateapipb.Worker{
		Metadata:        &ateapipb.ResourceMetadata{Name: workerName},
		WorkerNamespace: "default",
		WorkerPool:      "pool1",
		WorkerPod:       "worker-pod-1",
		WorkerPodUid:    workerName,
		NodeName:        "node1",
		Status:          &ateapipb.WorkerStatus{},
	}); err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}
	seedAssignment(t, persistence, workerName, &ateapipb.ActorAssignment{
		Actor:    &ateapipb.ObjectRef{Atespace: actorRef.Atespace, Name: actorRef.Name},
		ActorUid: created.GetMetadata().GetUid(),
	})

	w, _ := newWireCaptureWorkflow(t, workerDeletedDuringReleaseStore{Interface: persistence})
	_, err := w.SuspendActor(ctx, actorRef)
	assertConcurrentCrashKept(t, ctx, persistence, actorRef, err, "suspend failed: "+crashMessageWorkerPodGone)
}

func TestEnsureSuspendedFinalized_ReleasesOnlyOwnWorker(t *testing.T) {
	tests := []struct {
		name               string
		assignmentAtespace string
		mismatchedUID      bool
		wantReleased       bool
	}{
		{
			name:               "frees worker assigned to this actor",
			assignmentAtespace: "team-a",
			wantReleased:       true,
		},
		{
			name:               "keeps worker assigned to same-named actor in another atespace",
			assignmentAtespace: "team-b",
			wantReleased:       false,
		},
		{
			name:               "keeps worker assigned to previous incarnation of same actor",
			assignmentAtespace: "team-a",
			mismatchedUID:      true,
			wantReleased:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			persistence := newTestPersistence(t)
			seedSubstrateTemplate(t, ctx, persistence, "sub-tmpl")
			actor := &ateapipb.Actor{
				Metadata:      &ateapipb.ResourceMetadata{Atespace: "team-a", Name: "shared"},
				ActorTemplate: &ateapipb.ObjectRef{Atespace: "team-a", Name: "sub-tmpl"},
				Status: &ateapipb.ActorStatus{
					State: ateapipb.ActorState_ACTOR_STATE_SUSPENDING,
					WorkerAssignment: &ateapipb.WorkerAssignment{
						Worker:          &ateapipb.ObjectRef{Name: testWorkerUID("pod-1")},
						WorkerNamespace: "worker-ns",
						WorkerPool:      "pool",
						WorkerPod:       "pod-1",
						WorkerPodUid:    testWorkerUID("pod-1"),
						NodeName:        "node-1",
					},
					LastAssignedGeneration: 1,
					Snapshots: []*ateapipb.Snapshot{
						newDurableSnapshot(1, ateapipb.SnapshotOwner_SNAPSHOT_OWNER_ACTOR, ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY, "", "snapshot-1", someActorSnapshotURI(t, testStorageLocation, "team-a", "snapshot-1"), ateapipb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_PENDING),
					},
				},
			}
			created := storetest.MustCreateActor(t, ctx, persistence, actor)

			uid := created.GetMetadata().GetUid()
			if tt.assignmentAtespace != "team-a" || tt.mismatchedUID {
				uid = "other-actor-uid-b"
			}
			worker := &ateapipb.Worker{
				Metadata:        &ateapipb.ResourceMetadata{Name: testWorkerUID("pod-1")},
				WorkerNamespace: "worker-ns",
				WorkerPool:      "pool",
				WorkerPod:       "pod-1",
				WorkerPodUid:    testWorkerUID("pod-1"),
				Status:          &ateapipb.WorkerStatus{},
			}
			if _, err := persistence.CreateWorker(ctx, worker); err != nil {
				t.Fatalf("CreateWorker: %v", err)
			}
			seedAssignment(t, persistence, testWorkerUID("pod-1"), &ateapipb.ActorAssignment{
				Actor:    &ateapipb.ObjectRef{Atespace: tt.assignmentAtespace, Name: "shared"},
				ActorUid: uid,
			})

			w, _ := newWireCaptureWorkflow(t, persistence)
			if _, err := w.SuspendActor(ctx, resources.ActorRef{Atespace: "team-a", Name: "shared"}); err != nil {
				t.Fatalf("SuspendActor: %v", err)
			}

			stored := firstAssignment(t, persistence, testWorkerUID("pod-1"))
			if released := stored == nil; released != tt.wantReleased {
				t.Errorf("worker released = %t, want %t (assignment: %v)", released, tt.wantReleased, stored)
			}
		})
	}
}

// TestPreferredFidelity verifies golden actors always commit Full — new
// actors resume the golden snapshot Full, so the template's preferredFidelity must not
// thin it down to a data-only capture.
func TestPreferredFidelity(t *testing.T) {
	tmpl := func(fidelity ateapipb.SnapshotFidelity) *ateapipb.ActorTemplate {
		return &ateapipb.ActorTemplate{
			SnapshotConfig: &ateapipb.SnapshotConfig{PreferredFidelity: fidelity},
		}
	}
	fullScope := ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY
	dataScope := ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES
	tests := []struct {
		name     string
		atespace string
		fidelity ateapipb.SnapshotFidelity
		want     ateapipb.SnapshotFidelity
	}{
		{"golden actor ignores Data fidelity", resources.GoldenActorAtespace, dataScope, fullScope},
		{"golden actor keeps Full fidelity", resources.GoldenActorAtespace, fullScope, fullScope},
		{"regular actor uses Data fidelity", "team-a", dataScope, dataScope},
		{"regular actor uses Full fidelity", "team-a", fullScope, fullScope},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := preferredFidelity(tc.atespace, tmpl(tc.fidelity)); got != tc.want {
				t.Errorf("preferredFidelity(%q, fidelity=%s) = %s, want %s", tc.atespace, tc.fidelity, got, tc.want)
			}
		})
	}
}

// TestIsPausedOriginSuspend pins the paused-origin discriminator: a local
// snapshot alone must not select the paused path, because a RUNNING actor
// resumed from pause still carries one; only the paused shape (SUSPENDED with
// a node-local latest snapshot), or SUSPENDING with no worker assignment,
// means the suspend uploads a local snapshot.
func TestIsPausedOriginSuspend(t *testing.T) {
	assignment := &ateapipb.WorkerAssignment{WorkerNamespace: "ns", WorkerPool: "pool", WorkerPod: "pod-1"}
	localSnaps := []*ateapipb.Snapshot{
		newLocalSnapshot(1, ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY, "", "snap", "node1"),
	}
	tests := []struct {
		name  string
		actor *ateapipb.Actor
		want  bool
	}{
		{"paused actor", &ateapipb.Actor{Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_SUSPENDED, AssignedNode: "node1", LastAssignedGeneration: 1, Snapshots: localSnaps}}, true},
		{"suspended actor without a local snapshot", &ateapipb.Actor{Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_SUSPENDED}}, false},
		{"suspending retry of a paused-origin suspend", &ateapipb.Actor{Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_SUSPENDING, AssignedNode: "node1", Snapshots: localSnaps}}, true},
		{"running actor resumed from pause", &ateapipb.Actor{Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING, Snapshots: localSnaps, WorkerAssignment: assignment}}, false},
		{"suspending retry of a running-origin suspend after a resume from pause", &ateapipb.Actor{Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_SUSPENDING, Snapshots: localSnaps, WorkerAssignment: assignment}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPausedOriginSuspend(tc.actor); got != tc.want {
				t.Errorf("isPausedOriginSuspend = %t, want %t", got, tc.want)
			}
		})
	}
}

// TestEnsurePausedSnapshotUploaded_Preconditions covers the paused branch's
// failure handling: a lost node record crashes the actor (the snapshot can
// never be found), while an unreachable atelet stays retryable (the bytes
// are likely still on the node's disk).
func TestEnsurePausedSnapshotUploaded_Preconditions(t *testing.T) {
	t.Run("no node recorded crashes", func(t *testing.T) {
		ctx := context.Background()
		persistence := newTestPersistence(t)
		seedSubstrateTemplate(t, ctx, persistence, "sub-tmpl")
		w := &ActorWorkflow{store: persistence, dialer: newDanglingDialer()}

		snap := newDurableSnapshot(1, ateapipb.SnapshotOwner_SNAPSHOT_OWNER_ACTOR, ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY, "", "snap", someActorSnapshotURI(t, testStorageLocation, "team-a", "snap-dest"), ateapipb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_PENDING)
		snap.Locality = "node1"
		storetest.MustCreateActor(t, ctx, persistence, &ateapipb.Actor{
			Metadata:      &ateapipb.ResourceMetadata{Atespace: "team-a", Name: "actor-1"},
			ActorTemplate: &ateapipb.ObjectRef{Atespace: "team-a", Name: "sub-tmpl"},
			Status: &ateapipb.ActorStatus{
				State:                  ateapipb.ActorState_ACTOR_STATE_SUSPENDING,
				LastAssignedGeneration: 1,
				Snapshots:              []*ateapipb.Snapshot{snap},
			},
		})

		_, err := w.SuspendActor(ctx, resources.ActorRef{Atespace: "team-a", Name: "actor-1"})
		if got := apierror.Code(err); got != codes.FailedPrecondition {
			t.Fatalf("apierror.Code(err) = %v, want %v for a missing node record (err: %v)", got, codes.FailedPrecondition, err)
		}

		stored, err := persistence.GetActor(ctx, resources.ActorRef{Atespace: "team-a", Name: "actor-1"})
		if err != nil {
			t.Fatalf("GetActor: %v", err)
		}
		if stored.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_CRASHED {
			t.Errorf("state = %v, want CRASHED", stored.GetStatus().GetState())
		}
	})

	t.Run("no atelet on node stays retryable", func(t *testing.T) {
		ctx := context.Background()
		persistence := newTestPersistence(t)
		seedSubstrateTemplate(t, ctx, persistence, "sub-tmpl")
		w := &ActorWorkflow{store: persistence, dialer: newDanglingDialer()}

		snap := newDurableSnapshot(1, ateapipb.SnapshotOwner_SNAPSHOT_OWNER_ACTOR, ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY, "", "snap", someActorSnapshotURI(t, testStorageLocation, "team-a", "snap-dest"), ateapipb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_PENDING)
		snap.Locality = "node1"
		storetest.MustCreateActor(t, ctx, persistence, &ateapipb.Actor{
			Metadata:      &ateapipb.ResourceMetadata{Atespace: "team-a", Name: "actor-1"},
			ActorTemplate: &ateapipb.ObjectRef{Atespace: "team-a", Name: "sub-tmpl"},
			Status: &ateapipb.ActorStatus{
				State:                  ateapipb.ActorState_ACTOR_STATE_SUSPENDING,
				AssignedNode:           "node1",
				LastAssignedGeneration: 1,
				Snapshots:              []*ateapipb.Snapshot{snap},
			},
		})

		_, err := w.SuspendActor(ctx, resources.ActorRef{Atespace: "team-a", Name: "actor-1"})
		if !errors.Is(err, ErrNoAteletOnNode) {
			t.Fatalf("SuspendActor = %v, want ErrNoAteletOnNode", err)
		}

		stored, err := persistence.GetActor(ctx, resources.ActorRef{Atespace: "team-a", Name: "actor-1"})
		if err != nil {
			t.Fatalf("GetActor: %v", err)
		}
		if stored.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_SUSPENDING {
			t.Errorf("state = %v, want SUSPENDING (retryable, not crashed)", stored.GetStatus().GetState())
		}
	})
}

// TestEnsureSuspendedFinalized_MissingDurableSnapshotClearsAssignment verifies
// that if the latest generation snapshot entry is missing when
// ensureSuspendedFinalized runs, the actor transitions to CRASHED and clears
// WorkerAssignment after releasing the worker rather than getting stuck in
// SUSPENDING, and the step fails.
func TestEnsureSuspendedFinalized_MissingDurableSnapshotClearsAssignment(t *testing.T) {
	st := newTestPersistence(t)
	ctx := context.Background()
	seedSubstrateTemplate(t, ctx, st, "sub-tmpl")
	actorRef := resources.ActorRef{Atespace: "team-a", Name: "actor-1"}
	records := crashRecords(t)

	workerName := testWorkerUID("worker-pod-1")
	created := storetest.MustCreateActor(t, ctx, st, &ateapipb.Actor{
		Metadata:      &ateapipb.ResourceMetadata{Atespace: actorRef.Atespace, Name: actorRef.Name},
		ActorTemplate: &ateapipb.ObjectRef{Atespace: "team-a", Name: "sub-tmpl"},
		Status: &ateapipb.ActorStatus{
			State:        ateapipb.ActorState_ACTOR_STATE_SUSPENDING,
			AssignedNode: "node1",
			WorkerAssignment: &ateapipb.WorkerAssignment{
				Worker:          &ateapipb.ObjectRef{Name: workerName},
				WorkerNamespace: "default",
				WorkerPool:      "pool1",
				WorkerPod:       "worker-pod-1",
				WorkerPodUid:    workerName,
				NodeName:        "node-1",
			},
			LastAssignedGeneration: 1,
			Snapshots: []*ateapipb.Snapshot{
				newDurableSnapshot(1, ateapipb.SnapshotOwner_SNAPSHOT_OWNER_ACTOR, ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY, "", "snapshot-1", someActorSnapshotURI(t, testStorageLocation, "team-a", "snapshot-1"), ateapipb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_PENDING),
			},
		},
	})
	if _, err := st.CreateWorker(ctx, &ateapipb.Worker{
		Metadata:        &ateapipb.ResourceMetadata{Name: workerName},
		WorkerNamespace: "default",
		WorkerPool:      "pool1",
		WorkerPod:       "worker-pod-1",
		WorkerPodUid:    workerName,
		NodeName:        "node1",
		Status:          &ateapipb.WorkerStatus{},
	}); err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}
	seedAssignment(t, st, workerName, &ateapipb.ActorAssignment{
		Actor:    &ateapipb.ObjectRef{Atespace: actorRef.Atespace, Name: actorRef.Name},
		ActorUid: created.GetMetadata().GetUid(),
	})

	w, atelet := newWireCaptureWorkflow(t, st)
	atelet.onCheckpoint = func(ctx context.Context, _ *ateletpb.CheckpointRequest) error {
		mustUpdateActorStatus(t, ctx, st, created, func(s *ateapipb.ActorStatus) {
			s.Snapshots = nil
		})
		return nil
	}
	if _, err := w.SuspendActor(ctx, actorRef); apierror.Code(err) != codes.FailedPrecondition {
		t.Fatalf("SuspendActor error = %v, want FailedPrecondition for the actor it crashed", err)
	}
	got, err := st.GetActor(ctx, actorRef)
	if err != nil {
		t.Fatalf("GetActor: %v", err)
	}
	if got.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_CRASHED {
		t.Errorf("state = %v, want CRASHED", got.GetStatus().GetState())
	}
	if msg, want := got.GetStatus().GetCrash().GetMessage(), "suspend failed: "+crashMessageDurableSnapshotMissing; msg != want {
		t.Errorf("crash message = %q, want %q", msg, want)
	}
	if got.GetStatus().GetWorkerAssignment() != nil {
		t.Errorf("WorkerAssignment = %v, want nil", got.GetStatus().GetWorkerAssignment())
	}
	if len(*records) != 1 {
		t.Fatalf("got %d crash records, want 1", len(*records))
	}
}

// TestSuspendAndPause_CharacteristicMatrix pins the end-to-end behavior of
// synchronous suspend (SuspendActor) and asynchronous suspend (PauseActor)
// across all actor states, re-entry shapes, and golden actors: the returned
// gRPC code, which atelet RPC is called (and with which UploadMode), and the
// final persisted ActorStatus.
func TestSuspendAndPause_CharacteristicMatrix(t *testing.T) {
	type ateletCall int
	const (
		ateletNone ateletCall = iota
		ateletCheckpointImmediate
		ateletCheckpointDeferred
		ateletPromote
	)

	type wantOutcome struct {
		code       codes.Code
		rpc        ateletCall
		state      ateapipb.ActorState
		localOnly  bool
		hasDurable bool
		node       string
	}

	durablePending := func(locality string) []*ateapipb.Snapshot {
		snap := newDurableSnapshot(1, ateapipb.SnapshotOwner_SNAPSHOT_OWNER_ACTOR, ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY, "", "snap-1", someActorSnapshotURI(t, testStorageLocation, "team-a", "snap-1"), ateapipb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_PENDING)
		snap.Locality = locality
		return []*ateapipb.Snapshot{snap}
	}
	durableCompleted := func() []*ateapipb.Snapshot {
		return []*ateapipb.Snapshot{
			newDurableSnapshot(1, ateapipb.SnapshotOwner_SNAPSHOT_OWNER_ACTOR, ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY, "", "snap-1", someActorSnapshotURI(t, testStorageLocation, "team-a", "snap-1"), ateapipb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_COMPLETED),
		}
	}
	localPending := func() []*ateapipb.Snapshot {
		return []*ateapipb.Snapshot{
			newLocalSnapshot(1, ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY, "", "snap-1", ""),
		}
	}
	localCompleted := func() []*ateapipb.Snapshot {
		return []*ateapipb.Snapshot{
			newLocalSnapshot(1, ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY, "", "snap-1", "node-1"),
		}
	}

	tests := []struct {
		name      string
		atespace  string
		status    *ateapipb.ActorStatus
		wantSync  wantOutcome
		wantAsync wantOutcome
	}{
		{
			name: "RUNNING actor",
			status: &ateapipb.ActorStatus{
				State:            ateapipb.ActorState_ACTOR_STATE_RUNNING,
				AssignedNode:     "node-1",
				WorkerAssignment: wireTestAssignment(),
			},
			wantSync: wantOutcome{
				code:       codes.OK,
				rpc:        ateletCheckpointImmediate,
				state:      ateapipb.ActorState_ACTOR_STATE_SUSPENDED,
				hasDurable: true,
			},
			wantAsync: wantOutcome{
				code:      codes.OK,
				rpc:       ateletCheckpointDeferred,
				state:     ateapipb.ActorState_ACTOR_STATE_SUSPENDED,
				localOnly: true,
				node:      "node-1",
			},
		},
		{
			name: "SUSPENDED actor with durable snapshot",
			status: &ateapipb.ActorStatus{
				State:                  ateapipb.ActorState_ACTOR_STATE_SUSPENDED,
				LastAssignedGeneration: 1,
				Snapshots:              durableCompleted(),
			},
			wantSync: wantOutcome{
				code:       codes.OK,
				rpc:        ateletNone,
				state:      ateapipb.ActorState_ACTOR_STATE_SUSPENDED,
				hasDurable: true,
			},
			wantAsync: wantOutcome{
				code:       codes.FailedPrecondition,
				rpc:        ateletNone,
				state:      ateapipb.ActorState_ACTOR_STATE_SUSPENDED,
				hasDurable: true,
			},
		},
		{
			name: "SUSPENDED actor with local snapshot (paused)",
			status: &ateapipb.ActorStatus{
				State:                  ateapipb.ActorState_ACTOR_STATE_SUSPENDED,
				AssignedNode:           "node-1",
				LastAssignedGeneration: 1,
				Snapshots:              localCompleted(),
			},
			wantSync: wantOutcome{
				code:       codes.OK,
				rpc:        ateletPromote,
				state:      ateapipb.ActorState_ACTOR_STATE_SUSPENDED,
				hasDurable: true,
			},
			wantAsync: wantOutcome{
				code:      codes.OK,
				rpc:       ateletNone,
				state:     ateapipb.ActorState_ACTOR_STATE_SUSPENDED,
				localOnly: true,
				node:      "node-1",
			},
		},
		{
			name: "SUSPENDING re-entry from running-origin sync suspend",
			status: &ateapipb.ActorStatus{
				State:                  ateapipb.ActorState_ACTOR_STATE_SUSPENDING,
				AssignedNode:           "node-1",
				WorkerAssignment:       wireTestAssignment(),
				LastAssignedGeneration: 1,
				Snapshots:              durablePending(""),
			},
			wantSync: wantOutcome{
				code:       codes.OK,
				rpc:        ateletCheckpointImmediate,
				state:      ateapipb.ActorState_ACTOR_STATE_SUSPENDED,
				hasDurable: true,
			},
			wantAsync: wantOutcome{
				code:  codes.FailedPrecondition,
				rpc:   ateletNone,
				state: ateapipb.ActorState_ACTOR_STATE_SUSPENDING,
				node:  "node-1",
			},
		},
		{
			name: "SUSPENDING re-entry from paused-origin sync suspend",
			status: &ateapipb.ActorStatus{
				State:                  ateapipb.ActorState_ACTOR_STATE_SUSPENDING,
				AssignedNode:           "node-1",
				LastAssignedGeneration: 1,
				Snapshots:              durablePending("node-1"),
			},
			wantSync: wantOutcome{
				code:       codes.OK,
				rpc:        ateletPromote,
				state:      ateapipb.ActorState_ACTOR_STATE_SUSPENDED,
				hasDurable: true,
			},
			wantAsync: wantOutcome{
				code:  codes.FailedPrecondition,
				rpc:   ateletNone,
				state: ateapipb.ActorState_ACTOR_STATE_SUSPENDING,
				node:  "node-1",
			},
		},
		{
			name: "SUSPENDING re-entry from async pause",
			status: &ateapipb.ActorStatus{
				State:                  ateapipb.ActorState_ACTOR_STATE_SUSPENDING,
				AssignedNode:           "node-1",
				WorkerAssignment:       wireTestAssignment(),
				LastAssignedGeneration: 1,
				Snapshots:              localPending(),
			},
			wantSync: wantOutcome{
				code:  codes.FailedPrecondition,
				rpc:   ateletNone,
				state: ateapipb.ActorState_ACTOR_STATE_SUSPENDING,
				node:  "node-1",
			},
			wantAsync: wantOutcome{
				code:      codes.OK,
				rpc:       ateletCheckpointDeferred,
				state:     ateapipb.ActorState_ACTOR_STATE_SUSPENDED,
				localOnly: true,
				node:      "node-1",
			},
		},
		{
			name:   "RESUMING actor",
			status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RESUMING},
			wantSync: wantOutcome{
				code:  codes.FailedPrecondition,
				rpc:   ateletNone,
				state: ateapipb.ActorState_ACTOR_STATE_RESUMING,
			},
			wantAsync: wantOutcome{
				code:  codes.FailedPrecondition,
				rpc:   ateletNone,
				state: ateapipb.ActorState_ACTOR_STATE_RESUMING,
			},
		},
		{
			name:   "CRASHED actor",
			status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_CRASHED},
			wantSync: wantOutcome{
				code:  codes.FailedPrecondition,
				rpc:   ateletNone,
				state: ateapipb.ActorState_ACTOR_STATE_CRASHED,
			},
			wantAsync: wantOutcome{
				code:  codes.FailedPrecondition,
				rpc:   ateletNone,
				state: ateapipb.ActorState_ACTOR_STATE_CRASHED,
			},
		},
		{
			name:   "DELETING actor",
			status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_DELETING},
			wantSync: wantOutcome{
				code:  codes.FailedPrecondition,
				rpc:   ateletNone,
				state: ateapipb.ActorState_ACTOR_STATE_DELETING,
			},
			wantAsync: wantOutcome{
				code:  codes.FailedPrecondition,
				rpc:   ateletNone,
				state: ateapipb.ActorState_ACTOR_STATE_DELETING,
			},
		},
		{
			name:     "RUNNING golden actor",
			atespace: resources.GoldenActorAtespace,
			status: &ateapipb.ActorStatus{
				State:            ateapipb.ActorState_ACTOR_STATE_RUNNING,
				AssignedNode:     "node-1",
				WorkerAssignment: wireTestAssignment(),
			},
			wantSync: wantOutcome{
				code:       codes.OK,
				rpc:        ateletCheckpointImmediate,
				state:      ateapipb.ActorState_ACTOR_STATE_SUSPENDED,
				hasDurable: true,
			},
			wantAsync: wantOutcome{
				code:  codes.FailedPrecondition,
				rpc:   ateletNone,
				state: ateapipb.ActorState_ACTOR_STATE_RUNNING,
				node:  "node-1",
			},
		},
	}

	for _, tc := range tests {
		for _, mode := range []struct {
			label string
			run   func(*ActorWorkflow, context.Context, resources.ActorRef) (*ateapipb.Actor, error)
			want  wantOutcome
		}{
			{
				label: "SuspendActor",
				run:   (*ActorWorkflow).SuspendActor,
				want:  tc.wantSync,
			},
			{
				label: "PauseActor",
				run:   (*ActorWorkflow).PauseActor,
				want:  tc.wantAsync,
			},
		} {
			t.Run(tc.name+"/"+mode.label, func(t *testing.T) {
				ctx := context.Background()
				persistence := newTestPersistence(t)
				seedSubstrateTemplate(t, ctx, persistence, "sub-tmpl")
				atespace := tc.atespace
				if atespace == "" {
					atespace = "team-a"
				} else if atespace != "team-a" {
					storetest.MustCreateAtespace(t, ctx, persistence, atespace)
				}

				actorRef := resources.ActorRef{Atespace: atespace, Name: "actor-1"}
				storetest.MustCreateActor(t, ctx, persistence, &ateapipb.Actor{
					Metadata:      &ateapipb.ResourceMetadata{Atespace: actorRef.Atespace, Name: actorRef.Name},
					ActorTemplate: &ateapipb.ObjectRef{Atespace: "team-a", Name: "sub-tmpl"},
					Status:        proto.CloneOf(tc.status),
				})

				w, atelet := newWireCaptureWorkflow(t, persistence)
				_, err := mode.run(w, ctx, actorRef)
				if got := apierror.Code(err); got != mode.want.code {
					t.Fatalf("apierror.Code(err) = %v, want %v (err: %v)", got, mode.want.code, err)
				}

				cpReq := atelet.checkpointRequest()
				prReq := atelet.promoteRequest()
				switch mode.want.rpc {
				case ateletNone:
					if cpReq != nil || prReq != nil {
						t.Errorf("atelet received unexpected RPC (checkpoint=%v, promote=%v), want none", cpReq, prReq)
					}
				case ateletCheckpointImmediate:
					if cpReq == nil || prReq != nil {
						t.Fatalf("atelet RPCs = (checkpoint=%v, promote=%v), want Checkpoint only", cpReq, prReq)
					}
					if got := cpReq.GetUploadConfig().GetMode(); got != ateletpb.UploadMode_UPLOAD_MODE_IMMEDIATE {
						t.Errorf("UploadConfig.Mode = %v, want UPLOAD_MODE_IMMEDIATE", got)
					}
				case ateletCheckpointDeferred:
					if cpReq == nil || prReq != nil {
						t.Fatalf("atelet RPCs = (checkpoint=%v, promote=%v), want Checkpoint only", cpReq, prReq)
					}
					if got := cpReq.GetUploadConfig().GetMode(); got != ateletpb.UploadMode_UPLOAD_MODE_DEFERRED {
						t.Errorf("UploadConfig.Mode = %v, want UPLOAD_MODE_DEFERRED", got)
					}
				case ateletPromote:
					if prReq == nil || cpReq != nil {
						t.Fatalf("atelet RPCs = (checkpoint=%v, promote=%v), want PromoteSnapshot only", cpReq, prReq)
					}
				}

				stored, err := persistence.GetActor(ctx, actorRef)
				if err != nil {
					t.Fatalf("GetActor: %v", err)
				}
				if got := stored.GetStatus().GetState(); got != mode.want.state {
					t.Errorf("stored state = %v, want %v", got, mode.want.state)
				}
				if got := stored.GetStatus().GetAssignedNode(); got != mode.want.node {
					t.Errorf("AssignedNode = %q, want %q", got, mode.want.node)
				}
				if got := latestSnapshotIsLocalOnly(stored.GetStatus()); got != mode.want.localOnly {
					t.Errorf("latestSnapshotIsLocalOnly = %v, want %v", got, mode.want.localOnly)
				}
				gotDurable := findLatestDurableSnapshot(stored.GetStatus(), ateapipb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_COMPLETED) != nil
				if gotDurable != mode.want.hasDurable {
					t.Errorf("has completed durable snapshot = %v, want %v", gotDurable, mode.want.hasDurable)
				}
			})
		}
	}
}
