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

package ateerrors

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	epb "google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// errorReasonsFromStatus extracts the ErrorInfo reasons carried by a gRPC
// status error, mirroring how the ateapi control plane classifies failures.
// It returns nil when err is not a status error or carries no ErrorInfo.
func errorReasonsFromStatus(err error) []string {
	st, ok := status.FromError(err)
	if !ok {
		return nil
	}
	var reasons []string
	for _, d := range st.Details() {
		if info, ok := d.(*epb.ErrorInfo); ok {
			reasons = append(reasons, info.GetReason())
		}
	}
	return reasons
}

// TestNewGRPCError verifies the message comes from err, the Reason and metadata
// come from the arguments, the Domain is the package constant, and that they
// round-trip through the gRPC status as an ErrorInfo detail.
func TestNewGRPCError(t *testing.T) {
	tests := []struct {
		name         string
		reason       Reason
		metadata     map[string]string
		wantReason   string
		wantMetadata map[string]string
	}{
		{
			name:         "actor retriable metadata",
			reason:       ReasonFaileSaveSnapshot,
			metadata:     ActorRetriableMetadata(),
			wantReason:   string(ReasonFaileSaveSnapshot),
			wantMetadata: map[string]string{MetadataKeyActorRetriable: "true"},
		},
		{
			name:         "no metadata",
			reason:       ReasonInvalidCheckpointResult,
			metadata:     nil,
			wantReason:   string(ReasonInvalidCheckpointResult),
			wantMetadata: nil,
		},
		{
			name:         "empty reason defaults to UNSET",
			reason:       "",
			metadata:     nil,
			wantReason:   "UNSET",
			wantMetadata: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cause := errors.New("fetching manifest: snapshot missing")
			err := NewGRPCError(context.Background(), codes.NotFound, tt.reason, tt.metadata, cause)

			st, ok := status.FromError(err)
			if !ok {
				t.Fatalf("status.FromError(%v) = _, false; want a status error", err)
			}
			if got, want := st.Code(), codes.NotFound; got != want {
				t.Errorf("status code = %v, want %v", got, want)
			}
			if got, want := st.Message(), cause.Error(); got != want {
				t.Errorf("status message = %q, want %q", got, want)
			}

			// The reason must be extractable so the ateapi control plane can classify
			// the failure.
			if got := errorReasonsFromStatus(err); !slices.Contains(got, tt.wantReason) {
				t.Errorf("errorReasonsFromStatus() = %q, want it to contain %q", got, tt.wantReason)
			}

			var info *epb.ErrorInfo
			for _, d := range st.Details() {
				if v, ok := d.(*epb.ErrorInfo); ok {
					info = v
				}
			}
			if info == nil {
				t.Fatal("status is missing the ErrorInfo detail")
			}
			if got := info.GetReason(); got != tt.wantReason {
				t.Errorf("ErrorInfo.Reason = %q, want %q", got, tt.wantReason)
			}
			if diff := cmp.Diff(tt.wantMetadata, info.GetMetadata(), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("ErrorInfo.Metadata mismatch (-want +got):\n%s", diff)
			}
			// NewGRPCError stamps the package Domain into the ErrorInfo.
			if got, want := info.GetDomain(), errorDomain; got != want {
				t.Errorf("ErrorInfo.Domain = %q, want %q", got, want)
			}
		})
	}
}

// TestNewGRPCErrorInvalidInput verifies that a nil err or an OK code yields a
// plain validation error (not a gRPC status, so it carries no Reason or crash
// directive that the control plane could misclassify).
func TestNewGRPCErrorInvalidInput(t *testing.T) {
	tests := []struct {
		name     string
		grpcCode codes.Code
		err      error
	}{
		{name: "nil err", grpcCode: codes.NotFound, err: nil},
		{name: "OK code with valid err", grpcCode: codes.OK, err: errors.New("boom")},
		{name: "OK code with nil err", grpcCode: codes.OK, err: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := NewGRPCError(context.Background(), tt.grpcCode, ReasonInvalidCheckpointResult, nil, tt.err)
			if err == nil {
				t.Fatalf("NewGRPCError(%v, nil, %v) = nil, want a validation error", tt.grpcCode, tt.err)
			}
			// The validation error is a plain error, not a gRPC status, and it must
			// not carry a classifiable Reason or retriable directive.
			if _, ok := status.FromError(err); ok {
				t.Errorf("NewGRPCError(...) = %v; want a plain error, not a gRPC status", err)
			}
			if got := errorReasonsFromStatus(err); len(got) != 0 {
				t.Errorf("errorReasonsFromStatus() = %q, want no reasons", got)
			}
			if ActorRetryAllowed(err) {
				t.Errorf("ActorRetryAllowed(%v) = true, want false", err)
			}
		})
	}
}

// TestReasonTagging verifies a Reason is itself an error: the layer that knows
// the domain meaning of a failure wraps it with %w, and callers recover it with
// errors.Is (a specific Reason) or errors.As (any Reason).
func TestReasonTagging(t *testing.T) {
	err := fmt.Errorf("%w: while reading record: %w", ReasonFailedGetExternalObject, errors.New("eof"))
	if !errors.Is(err, ReasonFailedGetExternalObject) {
		t.Errorf("errors.Is(%v, ReasonFailedGetExternalObject) = false, want true", err)
	}
	if errors.Is(err, ReasonInvalidSandboxAsset) {
		t.Errorf("errors.Is(%v, ReasonInvalidSandboxAsset) = true, want false", err)
	}
	var r Reason
	if !errors.As(err, &r) {
		t.Fatalf("errors.As(%v, *Reason) = false, want true", err)
	}
	if r != ReasonFailedGetExternalObject {
		t.Errorf("errors.As recovered Reason %q, want %q", r, ReasonFailedGetExternalObject)
	}
}

// TestNewRetriableError verifies the boundary rule under crash-by-default: a
// call site that claims a failure as retriable produces a gRPC status with
// the claimed Reason and the actor-retriable directive.
func TestNewRetriableError(t *testing.T) {
	tagged := fmt.Errorf("%w: while fetching manifest: %w", ReasonObjectStorageUnavailable, errors.New("503"))
	err := NewRetriableError(context.Background(), codes.Unavailable, ReasonObjectStorageUnavailable, tagged)

	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("NewRetriableError(tagged) = %v, want a gRPC status error", err)
	}
	if got, want := st.Code(), codes.Unavailable; got != want {
		t.Errorf("status code = %v, want %v", got, want)
	}
	if got := errorReasonsFromStatus(err); !slices.Contains(got, string(ReasonObjectStorageUnavailable)) {
		t.Errorf("errorReasonsFromStatus() = %q, want it to contain %q", got, ReasonObjectStorageUnavailable)
	}
	if !ActorRetryAllowed(err) {
		t.Errorf("ActorRetryAllowed(%v) = false, want true", err)
	}
}

// TestAttachReason verifies that a wrapped Reason is promoted into an
// ErrorInfo detail so it survives the RPC boundary, without adding any
// retriable directive, and that already-classified or untagged errors pass
// through unchanged.
func TestAttachReason(t *testing.T) {
	t.Run("tagged error gains ErrorInfo but no exemption", func(t *testing.T) {
		tagged := fmt.Errorf("%w: while parsing manifest: %w", ReasonInvalidSandboxAsset, errors.New("bad json"))
		err := AttachReason(context.Background(), tagged)

		st, ok := status.FromError(err)
		if !ok {
			t.Fatalf("AttachReason(tagged) = %v, want a gRPC status error", err)
		}
		if got, want := st.Code(), codes.DataLoss; got != want {
			t.Errorf("status code = %v, want %v", got, want)
		}
		if got, want := ExtractReason(err), string(ReasonInvalidSandboxAsset); got != want {
			t.Errorf("ExtractReason(%v) = %q, want %q", err, got, want)
		}
		if ActorRetryAllowed(err) {
			t.Errorf("ActorRetryAllowed(%v) = true, want false", err)
		}
	})

	t.Run("existing status error passes through unchanged", func(t *testing.T) {
		downstream := status.Error(codes.Unavailable, "ateom draining")
		if got := AttachReason(context.Background(), downstream); got != downstream {
			t.Errorf("AttachReason(status) = %v, want the same error back", got)
		}
	})

	t.Run("unlisted reason passes through unchanged", func(t *testing.T) {
		tagged := fmt.Errorf("%w: boom", Reason("UNLISTED_DYNAMIC_ERROR_STRING"))
		if got := AttachReason(context.Background(), tagged); got != tagged {
			t.Errorf("AttachReason(unlisted) = %v, want the same error back", got)
		}
	})

	t.Run("untagged and nil pass through", func(t *testing.T) {
		plain := errors.New("boom")
		if got := AttachReason(context.Background(), plain); got != plain {
			t.Errorf("AttachReason(plain) = %v, want the same error back", got)
		}
		if got := AttachReason(context.Background(), nil); got != nil {
			t.Errorf("AttachReason(nil) = %v, want nil", got)
		}
	})
}

func TestErrorReasonsFromStatus(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want []string
	}{
		{name: "nil error", err: nil, want: nil},
		{name: "plain error without status", err: errors.New("boom"), want: nil},
		{name: "status without error info", err: status.Error(codes.Unavailable, "transient"), want: nil},
		{
			name: "grpc error carries reason",
			err:  NewGRPCError(context.Background(), codes.NotFound, ReasonFaileSaveSnapshot, nil, errors.New("boom")),
			want: []string{string(ReasonFaileSaveSnapshot)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// slices.Equal treats nil and empty as equal, which is the intent here:
			// "no reasons" may surface as either.
			if got := errorReasonsFromStatus(tt.err); !slices.Equal(got, tt.want) {
				t.Errorf("errorReasonsFromStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestActorRetryAllowed pins down the crash-by-default judgment: only the
// explicit retriable directive, canonically transient gRPC codes, and local
// context errors are exempt; everything else crashes the actor.
func TestActorRetryAllowed(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil error", err: nil, want: false},
		{name: "plain error without status", err: errors.New("boom"), want: false},
		{name: "context canceled", err: fmt.Errorf("while checkpointing: %w", context.Canceled), want: true},
		{name: "context deadline exceeded", err: context.DeadlineExceeded, want: true},
		{name: "transient code Unavailable", err: status.Error(codes.Unavailable, "connection refused"), want: true},
		{name: "transient code Aborted", err: status.Error(codes.Aborted, "conflict"), want: true},
		{name: "transient code ResourceExhausted", err: status.Error(codes.ResourceExhausted, "quota"), want: true},
		{name: "transient code DeadlineExceeded", err: status.Error(codes.DeadlineExceeded, "slow"), want: true},
		{name: "transient code Canceled", err: status.Error(codes.Canceled, "gone"), want: true},
		{name: "Internal crashes", err: status.Error(codes.Internal, "unclassified ateom failure"), want: false},
		{name: "Unknown crashes", err: status.Error(codes.Unknown, "boom"), want: false},
		{name: "DataLoss crashes", err: status.Error(codes.DataLoss, "snapshot gone"), want: false},
		{name: "NotFound crashes", err: status.Error(codes.NotFound, "no such workload"), want: false},
		{name: "InvalidArgument crashes", err: status.Error(codes.InvalidArgument, "bad request"), want: false},
		{name: "FailedPrecondition crashes", err: status.Error(codes.FailedPrecondition, "wrong state"), want: false},
		{
			name: "retriable directive exempts a crash-worthy code",
			err:  NewRetriableError(context.Background(), codes.Internal, ReasonObjectStorageUnavailable, errors.New("boom")),
			want: true,
		},
		{
			name: "reason without directive does not exempt",
			err:  NewGRPCError(context.Background(), codes.DataLoss, ReasonInvalidCheckpointResult, nil, errors.New("boom")),
			want: false,
		},
		{
			name: "metadata without retriable key does not exempt",
			err:  NewGRPCError(context.Background(), codes.DataLoss, ReasonInvalidCheckpointResult, map[string]string{"other": "x"}, errors.New("boom")),
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ActorRetryAllowed(tt.err); got != tt.want {
				t.Errorf("ActorRetryAllowed(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestExtractReason_EnforcesAllowedEnumValuesOnly(t *testing.T) {
	t.Run("valid enum reason returned", func(t *testing.T) {
		err := NewGRPCError(context.Background(), codes.DataLoss, ReasonFaileSaveSnapshot, nil, errors.New("boom"))
		if got := ExtractReason(err); got != "FAILED_SAVE_SNAPSHOT" {
			t.Errorf("ExtractReason(%v) = %q, want %q", err, got, "FAILED_SAVE_SNAPSHOT")
		}
	})

	t.Run("unlisted dynamic reason rejected to prevent metric high cardinality", func(t *testing.T) {
		err := NewGRPCError(context.Background(), codes.DataLoss, Reason("UNLISTED_DYNAMIC_ERROR_STRING"), nil, errors.New("boom"))
		if got := ExtractReason(err); got != "" {
			t.Errorf("ExtractReason(%v) = %q, want %q (empty string)", err, got, "")
		}
	})
}

func TestAllReasonsRegistered(t *testing.T) {
	if len(AllReasons) == 0 {
		t.Fatal("AllReasons slice is empty")
	}

	for _, r := range AllReasons {
		if !IsValidReason(string(r)) {
			t.Errorf("IsValidReason(%q) = false, want true", r)
		}
		err := NewGRPCError(context.Background(), codes.DataLoss, r, nil, errors.New("boom"))
		if got := ExtractReason(err); got != string(r) {
			t.Errorf("ExtractReason for %q = %q, want %q", r, got, r)
		}
	}
}
