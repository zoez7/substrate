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

package imagecache

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/agent-substrate/substrate/internal/ateerrors"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// timeoutErr implements net.Error with Timeout() = true.
type timeoutErr struct{ error }

func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// TestClassifyRegistryErr pins the hole-punching rule for image pulls: only
// failures the registry client's own classification calls temporary carry
// IMAGE_REGISTRY_UNAVAILABLE; deterministic pull failures stay untagged and
// crash the actor by default.
func TestClassifyRegistryErr(t *testing.T) {
	tests := []struct {
		name          string
		err           error
		wantTransient bool
	}{
		{"nil", nil, false},
		{"503 is transient", fmt.Errorf("while pulling: %w", &transport.Error{StatusCode: http.StatusServiceUnavailable}), true},
		{"429 is transient", fmt.Errorf("while pulling: %w", &transport.Error{StatusCode: http.StatusTooManyRequests}), true},
		{"network timeout is transient", fmt.Errorf("while resolving tag: %w", timeoutErr{errors.New("i/o timeout")}), true},
		{"404 is deterministic", fmt.Errorf("while pulling: %w", &transport.Error{StatusCode: http.StatusNotFound}), false},
		{"403 is deterministic", fmt.Errorf("while pulling: %w", &transport.Error{StatusCode: http.StatusForbidden}), false},
		{"plain error is deterministic", errors.New("bad reference"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := errors.Is(classifyRegistryErr(tt.err), ateerrors.ReasonImageRegistryUnavailable)
			if got != tt.wantTransient {
				t.Errorf("tagged IMAGE_REGISTRY_UNAVAILABLE = %v, want %v (err: %v)", got, tt.wantTransient, tt.err)
			}
		})
	}
}
