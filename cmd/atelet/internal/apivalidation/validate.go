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

package apivalidation

import (
	"context"

	"go.ate.dev/substrate/internal/proto/ateletpb"
	"go.ate.dev/substrate/internal/resources"
	"k8s.io/apimachinery/pkg/api/operation"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// ValidateRequestActorSuspendRequest runs the generated validation for req.
func ValidateRequestActorSuspendRequest(ctx context.Context, req *ateletpb.RequestActorSuspendRequest) field.ErrorList {
	return Validate_RequestActorSuspendRequest(ctx, operation.Operation{Type: operation.Create}, nil, req, nil)
}

// ValidateReportActorCrashedRequest runs the generated validation for req.
func ValidateReportActorCrashedRequest(ctx context.Context, req *ateletpb.ReportActorCrashedRequest) field.ErrorList {
	return Validate_ReportActorCrashedRequest(ctx, operation.Operation{Type: operation.Create}, nil, req, nil)
}

// ValidateGetActorEgressPolicyRequest runs the generated validation for
// req.
func ValidateGetActorEgressPolicyRequest(ctx context.Context, req *ateletpb.GetActorEgressPolicyRequest) field.ErrorList {
	return Validate_GetActorEgressPolicyRequest(ctx, operation.Operation{Type: operation.Create}, nil, req, nil)
}

// ValidateMintActorCertificateRequest runs the generated validation for req.
func ValidateMintActorCertificateRequest(ctx context.Context, req *ateletpb.MintActorCertificateRequest) field.ErrorList {
	return Validate_MintActorCertificateRequest(ctx, operation.Operation{Type: operation.Create}, nil, req, nil)
}

// ValidateRunRequest runs the generated validation for req.
func ValidateRunRequest(ctx context.Context, req *ateletpb.RunRequest) field.ErrorList {
	return Validate_RunRequest(ctx, operation.Operation{Type: operation.Create}, nil, req, nil)
}

// ValidateTerminateRequest runs the generated validation for req.
func ValidateTerminateRequest(ctx context.Context, req *ateletpb.TerminateRequest) field.ErrorList {
	return Validate_TerminateRequest(ctx, operation.Operation{Type: operation.Create}, nil, req, nil)
}

// ValidateCheckpointRequest runs the generated validation for req.
func ValidateCheckpointRequest(ctx context.Context, req *ateletpb.CheckpointRequest) field.ErrorList {
	return Validate_CheckpointRequest(ctx, operation.Operation{Type: operation.Create}, nil, req, nil)
}

// ValidateRestoreRequest runs the generated validation for req.
func ValidateRestoreRequest(ctx context.Context, req *ateletpb.RestoreRequest) field.ErrorList {
	return Validate_RestoreRequest(ctx, operation.Operation{Type: operation.Create}, nil, req, nil)
}

// ValidatePromoteSnapshotRequest runs the generated validation for req.
func ValidatePromoteSnapshotRequest(ctx context.Context, req *ateletpb.PromoteSnapshotRequest) field.ErrorList {
	return Validate_PromoteSnapshotRequest(ctx, operation.Operation{Type: operation.Create}, nil, req, nil)
}

// ValidateDetachActorRequest runs the generated validation for req.
func ValidateDetachActorRequest(ctx context.Context, req *ateletpb.DetachActorRequest) field.ErrorList {
	return Validate_DetachActorRequest(ctx, operation.Operation{Type: operation.Create}, nil, req, nil)
}

// ValidateSetWorkerSandboxRuntimesRequest runs the generated validation for req.
func ValidateSetWorkerSandboxRuntimesRequest(ctx context.Context, req *ateletpb.SetWorkerSandboxRuntimesRequest) field.ErrorList {
	return Validate_SetWorkerSandboxRuntimesRequest(ctx, operation.Operation{Type: operation.Create}, nil, req, nil)
}

// ValidateRegisterWorkerRequest runs the generated validation for req.
func ValidateRegisterWorkerRequest(ctx context.Context, req *ateletpb.RegisterWorkerRequest) field.ErrorList {
	return Validate_RegisterWorkerRequest(ctx, operation.Operation{Type: operation.Create}, nil, req, nil)
}

func ValidateCustom_EgressGateway_Address(_ context.Context, _ operation.Operation, fldPath *field.Path, value, _ *string) field.ErrorList {
	return resources.ValidateHostPort(fldPath, *value)
}

func ValidateCustom_ActorSandboxRuntime_SandboxClass(_ context.Context, _ operation.Operation, fldPath *field.Path, value, _ *string) field.ErrorList {
	return resources.ValidateSandboxClass(fldPath, *value)
}

// ValidateCustom_ActorSandboxRuntime_Version rejects accepted_values on
// snapshot and assignment stamps, which record the single value the actor ran
// under rather than a worker's compatibility table.
func ValidateCustom_ActorSandboxRuntime_Version(_ context.Context, _ operation.Operation, fldPath *field.Path, value, _ *ateletpb.VersionedSandboxCompat) field.ErrorList {
	var errs field.ErrorList
	for i, a := range value.GetAttributes() {
		if len(a.GetAcceptedValues()) > 0 {
			errs = append(errs, field.Forbidden(fldPath.Child("attributes").Index(i).Child("accepted_values"), "accepted_values is only allowed on WorkerSandboxRuntime"))
		}
	}
	return errs
}

// ValidateCustom_AttributeEntry enforces uniqueness of accepted_values.value
// and rejects an accepted_values entry that repeats the attribute's primary
// value.
func ValidateCustom_AttributeEntry(_ context.Context, _ operation.Operation, fldPath *field.Path, value, _ *ateletpb.AttributeEntry) field.ErrorList {
	var errs field.ErrorList
	seen := make(map[string]struct{}, len(value.GetAcceptedValues()))
	for i, av := range value.GetAcceptedValues() {
		if av == nil {
			continue
		}
		v := av.GetValue()
		valPath := fldPath.Child("accepted_values").Index(i).Child("value")
		if value.GetValue() != "" && v == value.GetValue() {
			errs = append(errs, field.Invalid(valPath, v, "must not equal the attribute's primary value"))
		}
		if _, dup := seen[v]; dup {
			errs = append(errs, field.Duplicate(valPath, v))
		}
		seen[v] = struct{}{}
	}
	return errs
}

func ValidateCustom_WorkerSandboxClass_SandboxClass(_ context.Context, _ operation.Operation, fldPath *field.Path, value, _ *string) field.ErrorList {
	return resources.ValidateSandboxClass(fldPath, *value)
}

func ValidateCustom_ObjectStorage_SnapshotUri(_ context.Context, _ operation.Operation, fldPath *field.Path, value, _ *string) field.ErrorList {
	return validateSnapshotURI(fldPath, *value)
}

// validateSnapshotURI requires a snapshot URI that resources.ParseSnapshotURI
// accepts.
func validateSnapshotURI(fldPath *field.Path, uri string) field.ErrorList {
	if _, err := resources.ParseSnapshotURI(uri); err != nil {
		return field.ErrorList{field.Invalid(fldPath, uri, err.Error())}
	}
	return nil
}

// ateDeepEqual is the deep-equal function declarative validation's generated
// code calls by name; it delegates to resources.DeepEqual.
func ateDeepEqual[T any](a, b T) bool {
	return resources.DeepEqual(a, b)
}
