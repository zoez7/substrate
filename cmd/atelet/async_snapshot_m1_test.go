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

package main

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"go.ate.dev/substrate/cmd/atelet/internal/ateletpath"
	"go.ate.dev/substrate/cmd/atelet/internal/snapshottracker"
	"go.ate.dev/substrate/internal/apierror"
	"go.ate.dev/substrate/internal/proto/ateletpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"k8s.io/utils/clock"
	clocktesting "k8s.io/utils/clock/testing"
)

// TestM1_A1_CheckpointIdempotentWhenAlreadyCommitted verifies scenario A1 on
// atelet: when a Checkpoint request arrives for a snapshot (same SnapshotUuid
// and Generation) that has already been committed locally (or durably), atelet
// must return the committed ActorSnapshotState idempotently without calling
// worker.CheckpointWorkload a second time on a workload that was already
// stopped and removed.
func TestM1_A1_CheckpointIdempotentWhenAlreadyCommitted(t *testing.T) {
	for _, tc := range []struct {
		name string
		snap string
		mode ateletpb.UploadMode
	}{
		{name: "deferred_pause", snap: "pause-snap-1", mode: ateletpb.UploadMode_UPLOAD_MODE_DEFERRED},
		{name: "immediate_suspend", snap: "suspend-snap-1", mode: ateletpb.UploadMode_UPLOAD_MODE_IMMEDIATE},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := runPluginActor(t)
			snap := a.snapshot(tc.snap, 1)

			resp1, err := a.checkpoint(t, snap, tc.mode)
			if err != nil {
				t.Fatalf("first Checkpoint: %v", err)
			}

			// The first checkpoint stopped and destroyed the workload on the
			// worker, so any second CheckpointWorkload RPC fails with NotFound.
			var secondWorkerCall atomic.Bool
			a.worker.onCheckpointErr = func() error {
				secondWorkerCall.Store(true)
				return status.Error(codes.NotFound, "workload not found")
			}

			resp2, err := a.checkpoint(t, snap, tc.mode)
			if err != nil {
				t.Fatalf("retried Checkpoint(%s) error = %v (called worker again = %v), want idempotent success returning committed state",
					tc.snap, err, secondWorkerCall.Load())
			}
			if secondWorkerCall.Load() {
				t.Errorf("retried Checkpoint(%s) called worker.CheckpointWorkload a second time", tc.snap)
			}
			if !proto.Equal(resp2.GetState(), resp1.GetState()) {
				t.Errorf("retried Checkpoint state = %v, want %v", resp2.GetState(), resp1.GetState())
			}
		})
	}
}

// blockingUploadPassThroughStore blocks inside UploadLocalSnapshot until
// release is closed, then runs the real upload to object storage.
type blockingUploadPassThroughStore struct {
	inner   snapshottracker.Store
	started chan struct{}
	release chan struct{}
}

func (s *blockingUploadPassThroughStore) UploadLocalSnapshot(ctx context.Context, actorUID string, ref snapshottracker.SnapshotRef) (bool, error) {
	s.started <- struct{}{}
	select {
	case <-s.release:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	return s.inner.UploadLocalSnapshot(ctx, actorUID, ref)
}

// TestM1_A3_RestoreDuringDetachFallsBackToUploadedSnapshot verifies scenario A3
// on atelet: when a Restore arrives for a local snapshot while detach is in
// flight, BeginStart waits for the detach to finish (which uploads the snapshot
// to object storage and prunes the local copy). Once BeginStart unblocks,
// Restore must be able to fall back to the uploaded durable copy recorded in
// the snapshot tracker even if the caller's RestoreRequest omitted
// Snapshot.Object because ateapi had not yet received the upload report.
func TestM1_A3_RestoreDuringDetachFallsBackToUploadedSnapshot(t *testing.T) {
	a := runPluginActor(t)
	snap := a.snapshot("pause-snap-1", 1)

	store := &blockingUploadPassThroughStore{
		inner:   &trackerStore{s: a.s},
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	a.s.snapshots = snapshottracker.New(t.Context(), store, clock.RealClock{})

	if _, err := a.checkpoint(t, snap, ateletpb.UploadMode_UPLOAD_MODE_DEFERRED); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	// Start Detach, which begins uploading pause-snap-1 and blocks on store.release.
	detachErr := make(chan error, 1)
	go func() {
		_, err := a.s.snapshots.Detach(t.Context(), pluginActorUID)
		detachErr <- err
	}()
	select {
	case <-store.started:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for Detach upload to start")
	}

	// While Detach is in flight, ateapi sends Restore with the snapshot
	// reference. Restore blocks in BeginStart waiting for Detach to complete,
	// then falls back to the uploaded snapshot once the local copy is pruned.
	restoreErr := make(chan error, 1)
	go func() {
		_, err := a.s.Restore(t.Context(), &ateletpb.RestoreRequest{
			Atespace:              "ate-demo",
			ActorName:             "counter",
			ActorUid:              pluginActorUID,
			ActorTemplateAtespace: "default",
			ActorTemplateName:     "counter",
			WorkerPodUid:          pluginWorkerPodUID,
			SandboxRuntime:        runtimeFor(t, a.rec),
			Spec:                  a.spec,
			Snapshot:              snap,
		})
		restoreErr <- err
	}()

	time.Sleep(50 * time.Millisecond)
	close(store.release)

	if err := <-detachErr; err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if err := <-restoreErr; err != nil {
		t.Fatalf("Restore racing with Detach failed with %v (code %v), want fallback to the snapshot Detach just uploaded (%v)",
			err, apierror.Code(err), a.s.snapshots.State(pluginActorUID).GetLatestDurable())
	}
}

// TestM1_A5_CheckpointCleanupFailureAfterCommitDoesNotFailRPC verifies scenario
// A5: when post-checkpoint directory cleanup (resetActorDirs) fails after the
// snapshot has already been committed in the snapshot tracker, Checkpoint must
// not return a fatal error that causes ateapi to crash the actor and discard
// the committed snapshot.
func TestM1_A5_CheckpointCleanupFailureAfterCommitDoesNotFailRPC(t *testing.T) {
	a := runPluginActor(t)
	snap := a.snapshot("pause-snap-1", 1)

	// Plant a non-empty volume directory during CheckpointWorkload (after the
	// pre-checkpoint resetActorDirs has already run) so the post-commit
	// resetActorDirs at the end of Checkpoint fails with ENOTEMPTY.
	a.worker.onCheckpoint = func() {
		busyVol := filepath.Join(ateletpath.ExternalVolumeMountsDir(pluginActorUID), "mounted-vol", "keep")
		if err := os.MkdirAll(busyVol, 0o755); err != nil {
			t.Errorf("planting busy volume dir: %v", err)
		}
	}

	resp, err := a.checkpoint(t, snap, ateletpb.UploadMode_UPLOAD_MODE_DEFERRED)
	committed := a.s.snapshots.State(pluginActorUID).GetLatestLocal()
	if committed.GetSnapshot().GetSnapshotUuid() != "pause-snap-1" {
		t.Fatalf("LatestLocal = %v, want pause-snap-1 committed before post-checkpoint cleanup", committed)
	}
	if err != nil {
		t.Fatalf("Checkpoint returned error %v (code %v) after committing pause-snap-1; "+
			"post-commit resetActorDirs failure must not fail Checkpoint and crash the actor",
			err, apierror.Code(err))
	}
	if got := resp.GetState().GetLatestLocal().GetSnapshot().GetSnapshotUuid(); got != "pause-snap-1" {
		t.Errorf("Checkpoint response LatestLocal = %q, want pause-snap-1", got)
	}
}

// TestM1_A6_RestoreResetDirsFailureReturnsUnavailable verifies scenario A6 on
// atelet: when resetActorDirs fails during Restore before the workload is
// started, Restore must return Unavailable (matching the comment at the call
// site: "Not crashing the actor, because terminal errors here indicate problems
// with atelet, node or the disk itself") rather than an unclassified Unknown
// error that causes ateapi to crash the actor.
func TestM1_A6_RestoreResetDirsFailureReturnsUnavailable(t *testing.T) {
	a := runPluginActor(t)
	snap := a.snapshot("pause-snap-1", 1)
	if _, err := a.checkpoint(t, snap, ateletpb.UploadMode_UPLOAD_MODE_DEFERRED); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	// Plant a non-empty volume directory so resetActorDirs at the start of
	// Restore fails before the workload starts.
	busyVol := filepath.Join(ateletpath.ExternalVolumeMountsDir(pluginActorUID), "mounted-vol", "keep")
	if err := os.MkdirAll(busyVol, 0o755); err != nil {
		t.Fatalf("planting busy volume dir: %v", err)
	}

	_, err := a.s.Restore(t.Context(), &ateletpb.RestoreRequest{
		Atespace:              "ate-demo",
		ActorName:             "counter",
		ActorUid:              pluginActorUID,
		ActorTemplateAtespace: "default",
		ActorTemplateName:     "counter",
		WorkerPodUid:          pluginWorkerPodUID,
		SandboxRuntime:        runtimeFor(t, a.rec),
		Spec:                  a.spec,
		Snapshot:              snap,
	})
	if code := apierror.Code(err); code != codes.Unavailable {
		t.Fatalf("Restore error on resetActorDirs failure = %v (code %v), want Unavailable so ateapi does not crash the actor",
			err, code)
	}
}

// TestM1_C3_ConcurrentDuplicateCheckpointDoesNotCallWorkerTwice verifies
// scenario C3: when two concurrent Checkpoint RPCs arrive with the exact same
// SnapshotUuid and Generation (e.g. after an ateapi lease expiry and re-entry),
// atelet must not allow both calls to enter worker.CheckpointWorkload
// concurrently on the same running container.
func TestM1_C3_ConcurrentDuplicateCheckpointDoesNotCallWorkerTwice(t *testing.T) {
	a := runPluginActor(t)
	snap := a.snapshot("pause-snap-1", 1)

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var workerCalls atomic.Int32

	a.worker.onCheckpointErr = func() error {
		n := workerCalls.Add(1)
		if n == 1 {
			close(firstStarted)
			<-releaseFirst
			return nil
		}
		return status.Error(codes.FailedPrecondition, "concurrent CheckpointWorkload on the same running workload")
	}

	errCh1 := make(chan error, 1)
	go func() {
		_, err := a.checkpoint(t, snap, ateletpb.UploadMode_UPLOAD_MODE_DEFERRED)
		errCh1 <- err
	}()

	select {
	case <-firstStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for first Checkpoint to reach worker")
	}

	errCh2 := make(chan error, 1)
	go func() {
		_, err := a.checkpoint(t, snap, ateletpb.UploadMode_UPLOAD_MODE_DEFERRED)
		errCh2 <- err
	}()

	// Give the duplicate Checkpoint time to reach BeginCheckpoint / worker.
	time.Sleep(50 * time.Millisecond)
	close(releaseFirst)

	err1 := <-errCh1
	err2 := <-errCh2
	if err1 != nil {
		t.Fatalf("first Checkpoint failed: %v", err1)
	}
	if calls := workerCalls.Load(); calls != 1 {
		t.Fatalf("worker.CheckpointWorkload called %d times for duplicate concurrent Checkpoint(%s) (second err = %v), want 1",
			calls, snap.GetSnapshotUuid(), err2)
	}
	if err2 != nil && apierror.Code(err2) != codes.Aborted && apierror.Code(err2) != codes.Unavailable {
		t.Fatalf("second concurrent Checkpoint returned crashing error %v (code %v), want nil or non-crashing retryable status",
			err2, apierror.Code(err2))
	}
}

// lateFinishUploadStore simulates an in-flight background upload that is inside
// its object-store write when Terminate cancels its context, finishing the
// write right after Terminate returns unless Terminate waits for the in-flight
// operation to exit.
type lateFinishUploadStore struct {
	objects       *recordingObjectStorage
	uploadStarted chan struct{}
	allowPut      chan struct{}
	uploadExited  chan struct{}
}

func (s *lateFinishUploadStore) UploadLocalSnapshot(_ context.Context, _ string, ref snapshottracker.SnapshotRef) (bool, error) {
	defer close(s.uploadExited)
	s.uploadStarted <- struct{}{}
	<-s.allowPut
	// Simulate an in-flight PutObject completing right as the upload goroutine
	// unwinds.
	s.objects.mu.Lock()
	if s.objects.objects == nil {
		s.objects.objects = map[string][]byte{}
	}
	s.objects.objects[manifestKey(ref.Snapshot.GetSnapshotUuid())] = []byte(`{"sandboxClass":"gvisor"}`)
	s.objects.mu.Unlock()
	return true, nil
}

// TestM1_C5_TerminateWaitsForInFlightBackgroundUploadBeforeReturning verifies
// scenario C5: when Terminate is called while a background upload is in flight
// (as RevertActor or DeleteActor does before calling CleanupSnapshot on object
// storage), Terminate must wait for the canceled in-flight upload goroutine to
// finish before returning, so that an in-flight PutObject cannot land after
// CleanupSnapshot and leave orphan objects in object storage.
func TestM1_C5_TerminateWaitsForInFlightBackgroundUploadBeforeReturning(t *testing.T) {
	a := runPluginActor(t)
	snap := a.snapshot("pause-snap-1", 1)
	clk := clocktesting.NewFakeClock(time.Now())

	store := &lateFinishUploadStore{
		objects:       a.store,
		uploadStarted: make(chan struct{}, 1),
		allowPut:      make(chan struct{}),
		uploadExited:  make(chan struct{}),
	}
	a.s.snapshots = snapshottracker.New(t.Context(), store, clk,
		snapshottracker.WithUploadDelay(time.Second),
		snapshottracker.WithDetachDelay(time.Hour))
	a.s.snapshots.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = a.s.snapshots.Wait(ctx)
	})

	cpResp, err := a.checkpoint(t, snap, ateletpb.UploadMode_UPLOAD_MODE_DEFERRED)
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	stepToUpload(t, clk, cpResp.GetState().GetUploadDueTime().AsTime(), store.uploadStarted)

	// Unblock the in-flight PutObject after 50ms. If Terminate waits for the
	// in-flight upload to finish, Terminate will return only after uploadExited
	// is closed.
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(store.allowPut)
	}()

	if _, err := a.s.Terminate(t.Context(), &ateletpb.TerminateRequest{
		Atespace:              "ate-demo",
		ActorName:             "counter",
		ActorUid:              pluginActorUID,
		ActorTemplateAtespace: "default",
		ActorTemplateName:     "counter",
		WorkerPodUid:          "",
		Spec:                  a.spec,
	}); err != nil {
		t.Fatalf("Terminate: %v", err)
	}

	// Simulate ateapi's ensureInProgressSnapshotDiscarded / CleanupSnapshot
	// running immediately after Terminate returns.
	a.store.mu.Lock()
	a.store.objects = nil
	a.store.mu.Unlock()

	<-store.uploadExited
	if leaked := a.store.keys(); len(leaked) != 0 {
		t.Fatalf("in-flight background upload wrote orphan objects %v after Terminate returned and CleanupSnapshot ran", leaked)
	}
}
