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
	"testing"
	"time"

	"go.ate.dev/substrate/cmd/ateapi/internal/scheduling"
	"go.ate.dev/substrate/cmd/ateapi/internal/store"
	"go.ate.dev/substrate/cmd/ateapi/internal/store/storetest"
	"go.ate.dev/substrate/cmd/ateapi/internal/workercache"
	"go.ate.dev/substrate/internal/apierror"
	"go.ate.dev/substrate/internal/ateattr"
	"go.ate.dev/substrate/internal/installdefaults"
	"go.ate.dev/substrate/internal/proto/ateletpb"
	"go.ate.dev/substrate/internal/resources"
	"go.ate.dev/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestM1_A1_SuspendRetryAfterCommitDoesNotCrash verifies scenario A1 on the
// control-plane side: when a first SuspendActor call commits the snapshot on
// atelet (or uploads it to object storage) and then fails transiently before
// FinalizeSuspended commits SUSPENDED, a retried SuspendActor with the same
// in-progress snapshot must complete and transition the actor to SUSPENDED
// rather than crashing the actor when the worker workload is already gone.
func TestM1_A1_SuspendRetryAfterCommitDoesNotCrash(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode suspendMode
	}{
		{name: "async_pause", mode: suspendAsync},
		{name: "sync_suspend", mode: suspendSync},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			persistence := newTestPersistence(t)
			seedSuspendTemplate(t, ctx, persistence, &ateapipb.SnapshotConfig{StorageLocation: testStorageLocation})
			w, atelet, objects := newWireFinalizeWorkflow(t, persistence)

			actorRef := resources.ActorRef{Atespace: "team-a", Name: "actor-1"}
			created := storetest.MustCreateActor(t, ctx, persistence, &ateapipb.Actor{
				Metadata:      &ateapipb.ResourceMetadata{Atespace: actorRef.Atespace, Name: actorRef.Name},
				ActorTemplate: suspendTestTemplateRef(),
				Status: &ateapipb.ActorStatus{
					State:            ateapipb.ActorState_ACTOR_STATE_RUNNING,
					AssignedNode:     "node-1",
					WorkerAssignment: wireTestAssignment(),
				},
			})
			seedWireWorker(t, ctx, persistence, assignmentOf(created))

			var attempt int
			atelet.onCheckpoint = func(context.Context) error {
				attempt++
				req := atelet.checkpointRequest()
				snapURI := mustParseSnapshotURI(t, req.GetSnapshot().GetObject().GetSnapshotUri())
				if attempt == 1 {
					// First attempt: atelet committed the snapshot (and for sync
					// mode uploaded manifest.json), but the RPC response was lost.
					if tc.mode == suspendSync {
						objects.PutSnapshot(t, snapURI, "manifest.json")
					}
					return apierror.Unavailable("connection reset after checkpoint committed")
				}
				// Second attempt: atelet answers from the already-committed
				// snapshot rather than calling the worker again.
				return nil
			}

			if _, err := w.SuspendActor(ctx, actorRef, tc.mode); apierror.Code(err) != codes.Unavailable {
				t.Fatalf("first SuspendActor error = %v, want Unavailable", err)
			}

			got, err := w.SuspendActor(ctx, actorRef, tc.mode)
			if err != nil {
				stored := mustReadActor(t, ctx, persistence, actorRef)
				t.Fatalf("retried SuspendActor error = %v (stored state = %v, crash = %q), want nil and ACTOR_STATE_SUSPENDED",
					err, stored.GetStatus().GetState(), stored.GetStatus().GetCrash().GetMessage())
			}
			if got.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_SUSPENDED {
				t.Fatalf("state = %v, want ACTOR_STATE_SUSPENDED", got.GetStatus().GetState())
			}
		})
	}
}

// TestM1_A2_StaleGenerationRejectedDoesNotCrashRunningActor verifies scenario
// A2: when atelet rejects a stale-generation Checkpoint or PromoteSnapshot with
// Aborted, ateapi must not crash the actor that is already running or paused at
// a newer generation.
func TestM1_A2_StaleGenerationRejectedDoesNotCrashRunningActor(t *testing.T) {
	staleErr := apierror.Aborted(`snapshot "stale-1" at generation 1 is below the actor's newest snapshot at generation 2`)

	t.Run("workflow_suspend", func(t *testing.T) {
		ctx := context.Background()
		persistence := newTestPersistence(t)
		seedSuspendTemplate(t, ctx, persistence, &ateapipb.SnapshotConfig{StorageLocation: testStorageLocation})
		w, atelet := newWireCaptureWorkflow(t, persistence)

		actorRef := resources.ActorRef{Atespace: "team-a", Name: "actor-1"}
		created := storetest.MustCreateActor(t, ctx, persistence, &ateapipb.Actor{
			Metadata:      &ateapipb.ResourceMetadata{Atespace: actorRef.Atespace, Name: actorRef.Name},
			ActorTemplate: suspendTestTemplateRef(),
			Status: &ateapipb.ActorStatus{
				State:            ateapipb.ActorState_ACTOR_STATE_RUNNING,
				AssignedNode:     "node-1",
				WorkerAssignment: wireTestAssignment(),
			},
		})
		seedWireWorker(t, ctx, persistence, assignmentOf(created))

		atelet.onCheckpoint = func(context.Context) error {
			return staleErr
		}

		_, err := w.SuspendActor(ctx, actorRef, suspendAsync)
		if apierror.Code(err) != codes.Aborted {
			t.Fatalf("SuspendActor error = %v, want Aborted", err)
		}
		stored := mustReadActor(t, ctx, persistence, actorRef)
		if stored.GetStatus().GetState() == ateapipb.ActorState_ACTOR_STATE_CRASHED {
			t.Fatalf("stored state = CRASHED (crash = %q), want actor not crashed on stale-generation rejection",
				stored.GetStatus().GetCrash().GetMessage())
		}
		if stored.GetStatus().GetWorkerAssignment() == nil {
			t.Errorf("WorkerAssignment cleared after stale-generation rejection, want preserved")
		}
	})
}

// TestM1_A3_ResumeRacesWithBackgroundDetach verifies scenarios A3 and A3':
//   - A3: An actor is paused (Locality="node-1", DurableSnapshot.Status=PENDING).
//     On the node, atelet has already uploaded the snapshot to object storage and
//     detached (deleted the local copy), but ReportActorSnapshotState has not yet
//     updated ateapi. When ResumeActor runs, it must pass Snapshot.Object in
//     RestoreRequest so atelet can fall back to the uploaded object copy, rather
//     than sending Object=nil and crashing the actor on NotFound.
//   - A3': If the actor is reverted while its latest pause snapshot is still
//     marked PENDING in ateapi (even though atelet already uploaded its
//     manifest.json and pruned the local copy), RevertActor must not delete the
//     only completed copy of that snapshot without verification.
func TestM1_A3_ResumeRacesWithBackgroundDetach(t *testing.T) {
	t.Run("A3_resume_sends_object_fallback_when_pending", func(t *testing.T) {
		ctx := context.Background()
		persistence := newTestPersistence(t)
		w, atelet, objects := newWireFinalizeWorkflow(t, persistence)

		storetest.MustCreateAtespace(t, ctx, persistence, "ns")
		tmpl, err := persistence.CreateActorTemplate(ctx, &ateapipb.ActorTemplate{
			Metadata:       &ateapipb.ResourceMetadata{Atespace: "ns", Name: "tmpl1"},
			SnapshotConfig: &ateapipb.SnapshotConfig{StorageLocation: testStorageLocation},
		})
		if err != nil {
			t.Fatalf("CreateActorTemplate: %v", err)
		}

		actorRef := resources.ActorRef{Atespace: "team-a", Name: "actor-a3"}
		seedWorkflowActor(t, ctx, persistence, actorRef, "ns", "tmpl1", ateapipb.ActorState_ACTOR_STATE_RESUMING, func(a *ateapipb.Actor) {
			a.Status.AssignedNode = "node-1"
			a.Status.WorkerAssignment = wireTestAssignment()
		})
		actor := mustReadActor(t, ctx, persistence, actorRef)
		pauseURI := mustActorSnapshotURI(t, tmpl, actor, "pause-snap-1")
		// atelet already finished uploading pause-snap-1 in the background before
		// detaching, so manifest.json exists in object storage.
		objects.PutSnapshot(t, pauseURI, "manifest.json")

		actor = mustUpdateActorStatus(t, ctx, persistence, actor, func(s *ateapipb.ActorStatus) {
			snap := newPausedSnapshot(t, 1, ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY, tmpl.GetMetadata().GetUid(), "pause-snap-1", "node-1")
			snap.DurableSnapshot = &ateapipb.SnapshotStorage{
				Status: ateapipb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_PENDING,
				Object: &ateapipb.ObjectSnapshot{SnapshotUri: pauseURI.String()},
			}
			s.LastAssignedGeneration = 1
			s.Snapshots = []*ateapipb.Snapshot{snap}
		})

		// Simulate atelet after background detach pruned the local copy: if
		// RestoreRequest carries Snapshot.Object, atelet falls back to object
		// storage and succeeds; if Object is nil, atelet returns NotFound.
		atelet.onRestore = func(_ context.Context, req *ateletpb.RestoreRequest) error {
			if req.GetSnapshot().GetObject().GetSnapshotUri() == "" {
				return status.Error(codes.NotFound, "local snapshot pause-snap-1 is not usable and request has no snapshot.object to fall back to")
			}
			return nil
		}

		loadedActor, loadedTmpl, src, err := w.loadActorForResume(ctx, actorRef)
		if err != nil {
			t.Fatalf("loadActorForResume: %v", err)
		}
		_, err = w.ensureAteletRestored(ctx, actorRef, loadedActor, loadedTmpl, nil, src)
		if err != nil {
			stored := mustReadActor(t, ctx, persistence, actorRef)
			t.Fatalf("ensureAteletRestored error = %v (stored state = %v, crash = %q), want nil with Snapshot.Object fallback populated",
				err, stored.GetStatus().GetState(), stored.GetStatus().GetCrash().GetMessage())
		}
		restoreReq, _ := atelet.requests()
		if got := restoreReq.GetSnapshot().GetObject().GetSnapshotUri(); got != pauseURI.String() {
			t.Errorf("RestoreRequest.Snapshot.Object.SnapshotUri = %q, want %q", got, pauseURI.String())
		}
	})

	t.Run("A3_prime_revert_destroys_uploaded_pause_snapshot_marked_pending", func(t *testing.T) {
		ctx := context.Background()
		persistence := newTestPersistence(t)
		tmpl := seedSubstrateTemplate(t, ctx, persistence, "sub-tmpl")
		w, objects := newFinalizeWorkflow(persistence)

		actor := storetest.MustCreateActor(t, ctx, persistence, &ateapipb.Actor{
			Metadata:      &ateapipb.ResourceMetadata{Atespace: "team-a", Name: "actor-a3-prime"},
			ActorTemplate: &ateapipb.ObjectRef{Atespace: "team-a", Name: "sub-tmpl"},
			Status:        &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_CRASHED},
		})
		pauseURI := mustActorSnapshotURI(t, tmpl, actor, "pause-snap-uploaded")
		// atelet completed the background upload (manifest.json is present) and
		// detached the local copy, but its report did not land before A3 crashed
		// the actor on Resume.
		objects.PutSnapshot(t, pauseURI, "manifest.json", "checkpoint.img.zstd")

		actor = mustUpdateActorStatus(t, ctx, persistence, actor, func(s *ateapipb.ActorStatus) {
			s.LastAssignedGeneration = 1
			s.Snapshots = []*ateapipb.Snapshot{
				newDurableSnapshot(1, ateapipb.SnapshotOwner_SNAPSHOT_OWNER_ACTOR, ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY, "", pauseURI.Name(), pauseURI.String(), ateapipb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_PENDING),
			}
		})

		actorRef := resources.ActorRefFromActor(actor)
		reverted, err := w.RevertActor(ctx, actorRef)
		if err != nil {
			t.Fatalf("RevertActor: %v", err)
		}
		// Because pause-snap-uploaded had a completed manifest.json in object
		// storage from the finished pause, reverting the crashed resume must
		// preserve that pause snapshot rather than wiping the actor back to cold
		// boot and deleting its only uploaded snapshot.
		if len(objects.Snapshot(t, pauseURI)) == 0 {
			t.Errorf("RevertActor deleted completed pause snapshot %s from object storage", pauseURI)
		}
		if got := findLatestDurableSnapshot(reverted.GetStatus(), ateapipb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_COMPLETED); got == nil {
			t.Errorf("reverted actor has no COMPLETED durable snapshot (snapshots = %v), want %s preserved",
				reverted.GetStatus().GetSnapshots(), pauseURI)
		}
	})
}

// TestM1_A6_RestoreNodeLocalResetFailureDoesNotCrashActor verifies scenario A6
// on the control-plane side: when atelet.Restore fails during node-local
// directory setup before the workload starts, ateapi must not crash the actor.
func TestM1_A6_RestoreNodeLocalResetFailureDoesNotCrashActor(t *testing.T) {
	ctx := context.Background()
	persistence := newTestPersistence(t)
	w, atelet := newWireCaptureWorkflow(t, persistence)

	storetest.MustCreateAtespace(t, ctx, persistence, "ns")
	tmpl, err := persistence.CreateActorTemplate(ctx, &ateapipb.ActorTemplate{
		Metadata:       &ateapipb.ResourceMetadata{Atespace: "ns", Name: "tmpl1"},
		SnapshotConfig: &ateapipb.SnapshotConfig{StorageLocation: testStorageLocation},
	})
	if err != nil {
		t.Fatalf("CreateActorTemplate: %v", err)
	}

	actorRef := resources.ActorRef{Atespace: "team-a", Name: "actor-a6"}
	seedWorkflowActor(t, ctx, persistence, actorRef, "ns", "tmpl1", ateapipb.ActorState_ACTOR_STATE_RESUMING, func(a *ateapipb.Actor) {
		a.Status.AssignedNode = "node-1"
		a.Status.WorkerAssignment = wireTestAssignment()
		a.Status.LastAssignedGeneration = 1
		a.Status.Snapshots = []*ateapipb.Snapshot{
			newPausedSnapshot(t, 1, ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY, tmpl.GetMetadata().GetUid(), "pause-snap-1", "node-1"),
		}
	})

	// Today atelet returns an unwrapped error (codes.Unknown) when resetActorDirs
	// fails before the workload starts.
	atelet.onRestore = func(context.Context, *ateletpb.RestoreRequest) error {
		return status.Error(codes.Unknown, "while resetting actor dirs: while removing volume dir: device or resource busy")
	}

	loadedActor, loadedTmpl, src, err := w.loadActorForResume(ctx, actorRef)
	if err != nil {
		t.Fatalf("loadActorForResume: %v", err)
	}
	_, _ = w.ensureAteletRestored(ctx, actorRef, loadedActor, loadedTmpl, nil, src)

	stored := mustReadActor(t, ctx, persistence, actorRef)
	if stored.GetStatus().GetState() == ateapipb.ActorState_ACTOR_STATE_CRASHED {
		t.Fatalf("stored state = CRASHED (crash = %q), want actor not crashed on node-local resetActorDirs failure",
			stored.GetStatus().GetCrash().GetMessage())
	}
}

// TestM1_A7_DetachAndPromoteNonFatalErrorsDoNotCrashActor verifies scenario A7:
// when Detach or PromoteSnapshot returns Aborted, FailedPrecondition, or
// Unimplemented, handleAteletError must not transition the actor to CRASHED.
func TestM1_A7_DetachAndPromoteNonFatalErrorsDoNotCrashActor(t *testing.T) {
	rpcs := []string{"Detach", "PromoteSnapshot"}
	nonFatalCodes := []codes.Code{
		codes.Aborted,
		codes.FailedPrecondition,
		codes.Unimplemented,
	}

	for _, rpc := range rpcs {
		for _, code := range nonFatalCodes {
			t.Run(rpc+"_"+code.String(), func(t *testing.T) {
				ctx := context.Background()
				st := newTestPersistence(t)
				actorRef := resources.ActorRef{Atespace: "team-a", Name: "actor-1"}
				seedActor(t, ctx, st, actorRef)
				seedWorker(t, ctx, st, actorRef)

				inErr := status.Error(code, "non-fatal snapshot operation precondition")
				_ = handleAteletError(ctx, st, actorRef, ateattr.OperationSuspend, rpc, false, inErr)

				actor, err := st.GetActor(ctx, actorRef)
				if err != nil {
					t.Fatalf("GetActor: %v", err)
				}
				if got := actor.GetStatus().GetState(); got != ateapipb.ActorState_ACTOR_STATE_RUNNING {
					t.Errorf("state after %s(%v) = %v (crash = %q), want ACTOR_STATE_RUNNING",
						rpc, code, got, actor.GetStatus().GetCrash().GetMessage())
				}
			})
		}
	}
}

// TestM1_B1_NodeLossWhileSuspendedWithLocalSnapshot verifies scenario B1:
// when the node holding a paused actor's local snapshot is gone
// (ErrNoAteletOnNode):
//   - B1a (local-only PENDING snapshot): RevertActor and DeleteActor must still
//     succeed without getting stuck on Terminate("node-gone").
//   - B1b (durable COMPLETED snapshot, but Locality and AssignedNode still point
//     to "node-gone"): ResumeActor must be able to schedule onto another healthy
//     node ("node-1") using the COMPLETED durable snapshot, and RevertActor /
//     DeleteActor must also succeed.
func TestM1_B1_NodeLossWhileSuspendedWithLocalSnapshot(t *testing.T) {
	t.Run("B1a_pending_local_only_revert_and_delete_succeed_when_node_gone", func(t *testing.T) {
		ctx := context.Background()
		persistence := newTestPersistence(t)
		tmpl := seedSubstrateTemplate(t, ctx, persistence, "sub-tmpl")
		w, _ := newFinalizeWorkflow(persistence)
		w.dialer = NewAteletDialer(newTestAteletIndexer(t), installdefaults.AteletSPIFFEID(installdefaults.SystemNamespace), "", "",
			WithNodeGetter(nodeGetter(nil, "node-1")))

		actor := storetest.MustCreateActor(t, ctx, persistence, &ateapipb.Actor{
			Metadata:      &ateapipb.ResourceMetadata{Atespace: "team-a", Name: "actor-b1a"},
			ActorTemplate: &ateapipb.ObjectRef{Atespace: "team-a", Name: "sub-tmpl"},
			Status: &ateapipb.ActorStatus{
				State:        ateapipb.ActorState_ACTOR_STATE_SUSPENDED,
				AssignedNode: "node-gone",
			},
		})
		pauseURI := mustActorSnapshotURI(t, tmpl, actor, "pause-snap-lost")
		mustUpdateActorStatus(t, ctx, persistence, actor, func(s *ateapipb.ActorStatus) {
			snap := newPausedSnapshot(t, 1, ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY, tmpl.GetMetadata().GetUid(), "pause-snap-lost", "node-gone")
			snap.DurableSnapshot.Object.SnapshotUri = pauseURI.String()
			s.LastAssignedGeneration = 1
			s.Snapshots = []*ateapipb.Snapshot{snap}
		})

		actorRef := resources.ActorRefFromActor(actor)
		if _, err := w.RevertActor(ctx, actorRef); err != nil {
			t.Errorf("RevertActor on dead node with PENDING local snapshot failed: %v, want success skipping dead node Terminate", err)
		}
		if _, err := w.DeleteActor(ctx, actorRef, store.DeletePreconditions{}); err != nil {
			t.Errorf("DeleteActor on dead node with PENDING local snapshot failed: %v, want success skipping dead node Terminate", err)
		}
	})

	t.Run("B1b_completed_durable_snapshot_resumes_on_another_node_when_assigned_node_gone", func(t *testing.T) {
		ctx := context.Background()
		persistence := newTestPersistence(t)
		w, _ := newWireCaptureWorkflow(t, persistence)

		storetest.MustCreateAtespace(t, ctx, persistence, "ns")
		tmpl, err := persistence.CreateActorTemplate(ctx, &ateapipb.ActorTemplate{
			Metadata:       &ateapipb.ResourceMetadata{Atespace: "ns", Name: "tmpl1"},
			SnapshotConfig: &ateapipb.SnapshotConfig{StorageLocation: testStorageLocation},
		})
		if err != nil {
			t.Fatalf("CreateActorTemplate: %v", err)
		}

		// Healthy worker is on node-1, while the actor is still pinned to
		// node-gone from before node-gone disappeared.
		if _, err := persistence.CreateWorker(ctx, &ateapipb.Worker{
			Metadata:        &ateapipb.ResourceMetadata{Name: "worker-1"},
			WorkerNamespace: "worker-ns",
			WorkerPool:      "pool",
			WorkerPod:       "pod-1",
			WorkerPodUid:    "worker-pod-uid",
			NodeName:        "node-1",
			Status: &ateapipb.WorkerStatus{
				State:          ateapipb.WorkerState_WORKER_STATE_ACTIVE,
				Capacity:       &ateapipb.WorkerResources{Actors: 1},
				SandboxClasses: workerSandboxClasses(testSandboxRuntime()),
			},
		}); err != nil {
			t.Fatalf("CreateWorker: %v", err)
		}
		cacheCtx, cancel := context.WithCancel(ctx)
		t.Cleanup(cancel)
		wc := workercache.New(persistence, time.Minute)
		if err := wc.Start(cacheCtx); err != nil {
			t.Fatalf("workercache.Start: %v", err)
		}
		w.workerCache = wc
		w.scheduler = scheduling.New(wc)

		actorRef := resources.ActorRef{Atespace: "team-a", Name: "actor-b1b"}
		seedWorkflowActor(t, ctx, persistence, actorRef, "ns", "tmpl1", ateapipb.ActorState_ACTOR_STATE_SUSPENDED, func(a *ateapipb.Actor) {
			a.Status.AssignedNode = "node-gone"
		})
		actor := mustReadActor(t, ctx, persistence, actorRef)
		durableURI := mustActorSnapshotURI(t, tmpl, actor, "pause-snap-durable")
		actor = mustUpdateActorStatus(t, ctx, persistence, actor, func(s *ateapipb.ActorStatus) {
			snap := newPausedSnapshot(t, 1, ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY, tmpl.GetMetadata().GetUid(), "pause-snap-durable", "node-gone")
			snap.DurableSnapshot = &ateapipb.SnapshotStorage{
				Status: ateapipb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_COMPLETED,
				Object: &ateapipb.ObjectSnapshot{SnapshotUri: durableURI.String()},
			}
			s.LastAssignedGeneration = 1
			s.Snapshots = []*ateapipb.Snapshot{snap}
		})

		resumed, _, err := w.ResumeActor(ctx, actorRef)
		if err != nil {
			t.Fatalf("ResumeActor when AssignedNode/Locality node-gone is dead but DurableSnapshot is COMPLETED failed: %v, want fallback to healthy node-1", err)
		}
		if got := resumed.GetStatus().GetState(); got != ateapipb.ActorState_ACTOR_STATE_RUNNING {
			t.Errorf("state = %v, want ACTOR_STATE_RUNNING", got)
		}
	})
}
