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

package snapshottracker

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.ate.dev/substrate/cmd/atelet/internal/ateletpath"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	clocktesting "k8s.io/utils/clock/testing"
)

// TestM1_A4_ReportRecoversAfterGiveUpWindow verifies scenario A4:
// when ateapi is unavailable for longer than DefaultReportGiveUpAfter during
// both the background upload report and the background detach report, the
// tracker must not permanently abandon reporting the detached/durable state
// once ateapi recovers (or across a tracker restart).
func TestM1_A4_ReportRecoversAfterGiveUpWindow(t *testing.T) {
	store := newFakeSnapshotStore()
	store.setBlocking(true)
	reporter := newFakeReporter()
	unavailable := status.Error(codes.Unavailable, "ateapi outage")

	// Both the upload report window and the detach report window experience
	// Unavailable past DefaultReportGiveUpAfter.
	reporter.failNext(unavailable, unavailable, unavailable, unavailable)
	tr := newReportingTracker(t, store, reporter)
	tr.CommitLocal(trackedActorUID, committedRef("s1", 1, tr.clock.Now()))

	// 1. Background upload completes; report fails past giveUpAfter.
	uploadNow(t, tr, store, nil)
	uploadDueAt := tr.clock.Now()
	processNextReport(t, tr)
	stepTo(tr, uploadDueAt.Add(DefaultReportGiveUpAfter+time.Minute))
	processNextReport(t, tr)

	// 2. Background detach completes; its report also fails past giveUpAfter.
	detachNow(t, tr)
	detachDueAt := tr.clock.Now()
	processNextReport(t, tr)
	stepTo(tr, detachDueAt.Add(DefaultReportGiveUpAfter+time.Minute))
	processNextReport(t, tr)

	// Now ateapi recovers (reporter has no more queued errors). Even after a
	// tracker restart or reconciliation interval, the unacknowledged detached
	// state must still be due for reporting so ateapi does not stay stuck with
	// Locality=node-1 and Status=PENDING forever.
	if !reportDue(tr) && !reportDueOnDisk() {
		t.Fatalf("reportDue = false and reportDueOnDisk = false after transient ateapi outage exceeded %v; "+
			"atelet permanently gave up reporting detached state %v",
			DefaultReportGiveUpAfter, tr.State(trackedActorUID))
	}
}

// TestM1_B2_MissingLocalSnapshotBeforeUploadDoesNotRetryForeverSilently
// verifies scenario B2: when the local snapshot directory on disk is missing or
// corrupted before the background upload runs and no remote copy exists in
// object storage, the tracker must treat the missing local snapshot as an
// unrecoverable error and report/surface the loss rather than backing off and
// retrying the upload forever with zero reports.
func TestM1_B2_MissingLocalSnapshotBeforeUploadDoesNotRetryForeverSilently(t *testing.T) {
	store := newFakeSnapshotStore()
	// UploadLocalSnapshot returns NotFound when both the local snapshot
	// directory and the remote manifest are absent.
	store.setErr(status.Error(codes.NotFound, `local snapshot "s1" is gone and no uploaded copy exists`))
	reporter := newFakeReporter()
	tr := newIdleTestTracker(t, store)
	setWorkers(tr, 0, 0)
	startReporting(tr, reporter)

	t0 := tr.clock.Now()
	tr.CommitLocal(trackedActorUID, committedRef("s1", 1, t0))

	// Advance to when the background upload is due and run one upload worker
	// pass.
	stepTo(tr, t0.Add(testUploadDelay))
	processNext(t, tr, opUpload)

	st := tr.State(trackedActorUID)
	if st.GetUploadDueTime() != nil {
		t.Errorf("UploadDueTime = %v after unrecoverable missing local snapshot, want cleared (not retried forever)",
			st.GetUploadDueTime().AsTime())
	}
	if !reportDue(tr) && len(reporter.reports()) == 0 {
		t.Errorf("reportDue = false and reports = 0 after unrecoverable missing local snapshot; " +
			"ateapi is never notified that the local-only snapshot was lost")
	}
}

// TestM1_B3_CorruptOrMissingStateFileReconcilesLocalSnapshotOnStartup verifies
// scenario B3: when a local checkpoint directory exists under
// local-checkpoint/<uuid> on disk, but snapshot-state.json is either missing
// (e.g., writing snapshot-state.json failed) or corrupted across an atelet
// restart, Tracker.Start() must reconcile/discover the on-disk local snapshot
// rather than silently skipping the actor and leaving the snapshot orphaned.
func TestM1_B3_CorruptOrMissingStateFileReconcilesLocalSnapshotOnStartup(t *testing.T) {
	for _, tc := range []struct {
		name      string
		prepState func(t *testing.T, stateFile string)
	}{
		{
			name: "missing_state_file_after_write_failure",
			prepState: func(t *testing.T, stateFile string) {
				_ = os.RemoveAll(stateFile)
			},
		},
		{
			name: "corrupt_state_file_json",
			prepState: func(t *testing.T, stateFile string) {
				if err := os.WriteFile(stateFile, []byte("{corrupt-json"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useTempNodeDirs(t)
			snapDir := ateletpath.LocalSnapshotDir(trackedActorUID, "s1")
			if err := os.MkdirAll(snapDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(snapDir, "manifest.json"), []byte(`{"sandboxClass":"gvisor"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			tc.prepState(t, ateletpath.ActorSnapshotStateFile(trackedActorUID))

			clk := clocktesting.NewFakeClock(time.Unix(1_700_000_000, 0))
			tr := New(t.Context(), newFakeSnapshotStore(), clk,
				WithUploadDelay(testUploadDelay),
				WithDetachDelay(testDetachDelay),
				WithUploadWorkers(0),
				WithDetachWorkers(0))
			tr.Start()

			st := tr.State(trackedActorUID)
			if st.GetLatestLocal() == nil && queuedNow(tr) == 0 {
				t.Fatalf("after Start() with %s and on-disk local snapshot %s: State = %v, queuedNow = 0; "+
					"local snapshot on disk was silently orphaned",
					tc.name, snapDir, st)
			}
		})
	}
}
