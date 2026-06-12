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

package router

import (
	"context"
	"time"

	"github.com/agent-substrate/substrate/internal/atemetadata"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"golang.org/x/sync/singleflight"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/util/wait"
)

// ActorResumer coordinates safe, deduplicated resumption of actors.
type ActorResumer struct {
	apiClient ateapipb.ControlClient
	flight    singleflight.Group
}

func NewActorResumer(apiClient ateapipb.ControlClient) *ActorResumer {
	return &ActorResumer{
		apiClient: apiClient,
	}
}

// ResumeActor ensures the requested actor is running. It deduplicates concurrent
// requests within the process and retries when needed.
//
// forwardedJWT, when non-empty, is the calling actor's session JWT; it is
// forwarded to the apiserver so the resume is authenticated on the actor's
// behalf. Because concurrent requests for the same actorID are deduplicated
// onto a single flight, callers that join an in-progress flight share the
// identity forwarded by the caller that started it.
func (r *ActorResumer) ResumeActor(ctx context.Context, actorID, forwardedJWT string) (*ateapipb.Actor, error) {
	ch := r.flight.DoChan(actorID, func() (interface{}, error) {
		// We detach the context from the first caller using a fixed background timeout.
		// This guarantees that if Caller 1 disconnects or times out, the underlying
		// resume operation continues running for Caller 2 and Caller 3 without failing.
		bgCtx, bgCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer bgCancel()

		if forwardedJWT != "" {
			bgCtx = metadata.AppendToOutgoingContext(bgCtx, atemetadata.ForwardedJWTKey, forwardedJWT)
		}

		backoff := wait.Backoff{
			Steps:    7,
			Duration: 200 * time.Millisecond,
			Factor:   1.5,
			Jitter:   0.2,
		}

		var resumeResp *ateapipb.ResumeActorResponse

		err := wait.ExponentialBackoffWithContext(bgCtx, backoff, func(ctx context.Context) (bool, error) {
			var err error
			// This call needs to pass the identity of the caller to the ate-api, instead of using router's identity.
			resumeResp, err = r.apiClient.ResumeActor(ctx, &ateapipb.ResumeActorRequest{
				ActorId: actorID,
			})
			if err == nil {
				return true, nil
			}

			if status.Code(err) == codes.Aborted {
				return false, nil // Concurrent resume call, retry.
			}
			// Other gRPC errors (NotFound, FailedPrecondition, Unavailable,
			// DeadlineExceeded, ...) are returned to the caller unchanged so
			// the HTTP boundary can map them with full fidelity.
			return false, err
		})

		if err != nil {
			return nil, err
		}

		return resumeResp.GetActor(), nil
	})

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		return res.Val.(*ateapipb.Actor), nil
	}
}
