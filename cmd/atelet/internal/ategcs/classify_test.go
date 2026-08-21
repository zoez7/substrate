// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ategcs

import (
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/agent-substrate/substrate/internal/ateerrors"
	"google.golang.org/api/googleapi"
)

// retryableS3Err implements the smithy retryable-error interface the AWS SDK's
// standard retryer consults.
type retryableS3Err struct{ error }

func (retryableS3Err) RetryableError() bool { return true }

// s3StatusErr carries an HTTP status code the way smithy response errors do.
type s3StatusErr struct {
	error
	code int
}

func (e s3StatusErr) HTTPStatusCode() int { return e.code }

// TestClassifyErrs pins the hole-punching rule for object storage: only
// failures the storage SDK's own retry predicate calls transient carry
// OBJECT_STORAGE_UNAVAILABLE; deterministic failures stay untagged and crash
// the actor by default.
func TestClassifyErrs(t *testing.T) {
	tests := []struct {
		name          string
		err           error
		wantTransient bool
	}{
		{"gcs 503 is transient", classifyGCSErr(fmt.Errorf("while reading: %w", &googleapi.Error{Code: 503})), true},
		{"gcs 429 is transient", classifyGCSErr(fmt.Errorf("while reading: %w", &googleapi.Error{Code: 429})), true},
		{"gcs unexpected EOF is transient", classifyGCSErr(fmt.Errorf("while reading: %w", io.ErrUnexpectedEOF)), true},
		{"gcs 403 is deterministic", classifyGCSErr(fmt.Errorf("while reading: %w", &googleapi.Error{Code: 403})), false},
		{"gcs plain error is deterministic", classifyGCSErr(errors.New("boom")), false},
		{"s3 sdk-retryable is transient", classifyS3Err(fmt.Errorf("while putting: %w", retryableS3Err{errors.New("throttled")})), true},
		{"s3 503 is transient", classifyS3Err(fmt.Errorf("while putting: %w", s3StatusErr{errors.New("service unavailable"), 503})), true},
		{"s3 403 is deterministic", classifyS3Err(fmt.Errorf("while putting: %w", s3StatusErr{errors.New("access denied"), 403})), false},
		{"s3 plain error is deterministic", classifyS3Err(errors.New("boom")), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := errors.Is(tt.err, ateerrors.ReasonObjectStorageUnavailable); got != tt.wantTransient {
				t.Errorf("tagged OBJECT_STORAGE_UNAVAILABLE = %v, want %v (err: %v)", got, tt.wantTransient, tt.err)
			}
		})
	}
}
