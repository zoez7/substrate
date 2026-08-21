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
	"log/slog"
	"slices"

	epb "google.golang.org/genproto/googleapis/rpc/errdetails"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// errorDomain is the AIP-193 ErrorInfo.errorDomain (https://google.aip.dev/193) stamped
// into every error built by NewGRPCError, identifying Agent Substrate as the
// source service.
const errorDomain = "substrate.dev"

// Reason is the AIP-193 ErrorInfo.Reason: a bounded, UPPER_SNAKE_CASE enum of
// failure causes the control plane can classify on. A Reason is also an error:
// source layers tag failures with fmt.Errorf("%w: ...", ReasonX, err), each RPC
// boundary claims the Reasons it treats as retriable (errors.Is +
// NewRetriableError) and surfaces the rest on the wire (AttachReason); errors
// not proven retriable crash the actor (ActorRetryAllowed).
type Reason string

// Error makes a Reason wrappable with %w and matchable with errors.Is/As.
func (r Reason) Error() string { return string(r) }

// NOTE: When adding a Reason constant below, also add it to AllReasons.
const (
	ReasonTerminalFileSystemError Reason = "TERMINAL_FILE_SYSTEM_ERROR"
	ReasonInvalidSandboxAsset     Reason = "INVALID_SANDBOX_ASSET"
	ReasonInvalidCheckpointResult Reason = "INVALID_CHECKPOINT_RESULT"
	ReasonFaileSaveSnapshot       Reason = "FAILED_SAVE_SNAPSHOT"
	ReasonInvalidObjectURL        Reason = "INVALID_OBJECT_URL"
	ReasonFailedGetExternalObject Reason = "FAILED_GET_EXTERNAL_OBJECT"
	// ReasonInvalidContainerConfig marks a container whose configuration cannot
	// produce a runnable process (e.g. the resolved argv is empty because the
	// image defines no ENTRYPOINT/CMD and the ActorTemplate sets no command/args).
	ReasonInvalidContainerConfig Reason = "INVALID_CONTAINER_CONFIG"

	// ReasonLocalSnapshotGone marks a paused actor whose local snapshot is
	// missing from the node it was recorded on and absent from object storage:
	// its state is unrecoverable.
	ReasonLocalSnapshotGone Reason = "LOCAL_SNAPSHOT_GONE"

	// ReasonObjectStorageUnavailable marks an object-storage failure that the
	// storage client's own retry predicate classifies as transient (connection
	// trouble, 408/429/5xx) — distinct from FAILED_GET_EXTERNAL_OBJECT, which
	// means the object is definitively gone. Deterministic failures (403, 400)
	// never carry it. Boundaries claim it (errors.Is + NewRetriableError) to
	// exempt retry-safe operations from the crash default.
	ReasonObjectStorageUnavailable Reason = "OBJECT_STORAGE_UNAVAILABLE"

	// ReasonImageRegistryUnavailable is OBJECT_STORAGE_UNAVAILABLE's twin for
	// image pulls: tagged iff the registry client's own classification calls
	// the failure temporary (429, 5xx, network timeout). Deterministic pull
	// failures (401/403/404, bad reference) never carry it.
	ReasonImageRegistryUnavailable Reason = "IMAGE_REGISTRY_UNAVAILABLE"

	// Control-plane failure reasons for ate.actor.crashes metric.
	ReasonCorruptedAssignment Reason = "CORRUPTED_ASSIGNMENT"
	ReasonWorkerReassigned    Reason = "WORKER_REASSIGNED"
	ReasonWorkerPodGone       Reason = "WORKER_POD_GONE"
	ReasonUnknown             Reason = "UNKNOWN"
)

// AllReasons contains all valid Reason constants for validation. Keep in sync with const block above.
var AllReasons = []Reason{
	ReasonTerminalFileSystemError,
	ReasonInvalidSandboxAsset,
	ReasonInvalidCheckpointResult,
	ReasonFaileSaveSnapshot,
	ReasonInvalidObjectURL,
	ReasonFailedGetExternalObject,
	ReasonInvalidContainerConfig,
	ReasonLocalSnapshotGone,
	ReasonObjectStorageUnavailable,
	ReasonImageRegistryUnavailable,
	ReasonCorruptedAssignment,
	ReasonWorkerReassigned,
	ReasonWorkerPodGone,
	ReasonUnknown,
}

// MetadataKeyActorRetriable marks (in ErrorInfo.Metadata) a failure the control
// plane may retry instead of crashing the actor. Errors crash the actor by
// default; this directive is the explicit exemption.
const MetadataKeyActorRetriable = "actorRetriable"

// ActorRetriableMetadata returns the AIP-193 metadata exempting a failure from
// the crash-by-default rule. The control plane reads it via ActorRetryAllowed.
func ActorRetriableMetadata() map[string]string {
	return map[string]string{MetadataKeyActorRetriable: "true"}
}

// NewGRPCError builds an internal gRPC status error per AIP-193
// (https://google.aip.dev/193#status-message), with a google.rpc.ErrorInfo detail
// carrying the given Reason ("UNSET" when empty).
// metadata carries additional structured directives such as ActorRetriableMetadata(),
// which the control plane reads via ActorRetryAllowed to decide whether the
// failure is exempt from the crash default.
func NewGRPCError(ctx context.Context, grpcCode codes.Code, reason Reason, metadata map[string]string, err error) error {
	// Validate the input parameters.
	if err == nil || grpcCode == codes.OK {
		return fmt.Errorf("cannot use NewGRPCError with OK error code or a nil err grpcCode=%v, err=%w. Return nil instead", grpcCode, err)
	}
	if reason == "" {
		reason = "UNSET"
	}
	st, derr := status.New(grpcCode, err.Error()).WithDetails(
		&epb.ErrorInfo{
			Domain:   errorDomain,
			Reason:   string(reason),
			Metadata: metadata,
		},
	)
	if derr != nil {
		// WithDetails on *epb.ErrorInfo should never fail; but if it ever does, the
		// reason and metadata are lost and the control plane will misclassify the
		// failure (e.g. a real crash read as a transient error). Log loudly for
		// debugging purpose.
		slog.ErrorContext(ctx, "ateerrors: failed to attach ErrorInfo to gRPC status; adding Reason/metadata to the error message instead",
			"err", derr, "reason", reason, "metadata", metadata, "code", grpcCode)
		return status.Error(grpcCode, fmt.Errorf("reason:%s metadata:%v, error %w", reason, metadata, err).Error())
	}
	return st.Err()
}

// NewRetriableError builds a gRPC status error carrying the actor-retriable
// directive: the failure is exempt from the crash-by-default rule and the
// actor stays in its in-progress state for a re-entered workflow.
// Claiming is per call site: the same tagged failure may be retriable in one
// RPC and crash the actor in another.
func NewRetriableError(ctx context.Context, grpcCode codes.Code, reason Reason, err error) error {
	return NewGRPCError(ctx, grpcCode, reason, ActorRetriableMetadata(), err)
}

// AttachReason promotes a Reason wrapped in err's chain into a DataLoss gRPC
// status with an ErrorInfo detail, so the classification survives the RPC
// boundary (the server interceptor masks non-status errors as Internal,
// dropping the tag). Errors that already are gRPC statuses — classified at a
// lower boundary, or raised by a downstream RPC — and untagged errors pass
// through unchanged. It carries no directive: whether the actor crashes is
// the control plane's default for non-retriable errors.
func AttachReason(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	r, ok := errors.AsType[Reason](err)
	if !ok || !IsValidReason(string(r)) {
		return err
	}
	return NewGRPCError(ctx, codes.DataLoss, r, nil, err)
}

// transientCodes are the gRPC codes that are canonically safe to retry
// (AIP-194): the operation either did not run or can be re-attempted by the
// idempotent, re-entrant workflows. Any other code crashes the actor unless
// the error carries the actor-retriable directive.
var transientCodes = []codes.Code{
	codes.Unavailable,
	codes.Aborted,
	codes.ResourceExhausted,
	codes.DeadlineExceeded,
	codes.Canceled,
}

// ActorRetryAllowed reports whether err is exempt from the crash-by-default
// rule: it carries the actorRetriable=true directive, a canonically transient
// gRPC code, or is a context cancellation/timeout that never left the caller.
func ActorRetryAllowed(err error) bool {
	if err == nil {
		return false
	}
	// Local context errors are not gRPC statuses but are always retriable: the
	// operation was abandoned by the caller, not failed by the callee.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	st, ok := status.FromError(err)
	if !ok {
		return false
	}
	if slices.Contains(transientCodes, st.Code()) {
		return true
	}
	for _, d := range st.Details() {
		if info, ok := d.(*epb.ErrorInfo); ok {
			if info.GetMetadata()[MetadataKeyActorRetriable] == "true" {
				return true
			}
		}
	}
	return false
}

// IsValidReason reports whether a string matches a known ateerrors.Reason enum.
func IsValidReason(s string) bool {
	return slices.Contains(AllReasons, Reason(s))
}

// ExtractReason returns the validated enum reason string from an error's AIP-193 ErrorInfo detail
// or wrapped ateerrors.Reason, or empty string if unclassified.
func ExtractReason(err error) string {
	if err == nil {
		return ""
	}
	var r Reason
	if errors.As(err, &r) && IsValidReason(string(r)) {
		return string(r)
	}
	st, ok := status.FromError(err)
	if ok {
		for _, d := range st.Details() {
			if info, ok := d.(*epb.ErrorInfo); ok {
				if rStr := info.GetReason(); rStr != "" && IsValidReason(rStr) {
					return rStr
				}
			}
		}
	}
	return ""
}
