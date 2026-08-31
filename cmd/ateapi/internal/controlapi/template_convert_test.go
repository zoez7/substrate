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
	"testing"

	"github.com/agent-substrate/substrate/cmd/ateapi/internal/store"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// seedSubstrateTemplate stores a minimal substrate ActorTemplate in team-a.
func seedSubstrateTemplate(t *testing.T, ctx context.Context, persistence store.Interface, name string) *ateapipb.ActorTemplate {
	t.Helper()
	stored, err := persistence.CreateActorTemplate(ctx, &ateapipb.ActorTemplate{
		Metadata: &ateapipb.ResourceMetadata{Atespace: "team-a", Name: name},
		SnapshotsConfig: &ateapipb.SnapshotsConfig{
			StorageLocation: "gs://ate-snapshots/team-a/",
		},
		SandboxConfig: &ateapipb.SandboxConfig{
			SandboxClass: ateapipb.SandboxClass_SANDBOX_CLASS_GVISOR,
			ConfigName:   "gvisor",
		},
	})
	if err != nil {
		t.Fatalf("CreateActorTemplate: %v", err)
	}
	return stored
}

// TestResolveActorTemplate verifies the resolver reads the substrate resource
// the actor's actor_template reference names.
func TestResolveActorTemplate(t *testing.T) {
	ctx := context.Background()
	persistence := newTestPersistence(t)
	stored := seedSubstrateTemplate(t, ctx, persistence, "sub-tmpl")

	t.Run("ref reads the store", func(t *testing.T) {
		actor := &ateapipb.Actor{ActorTemplate: &ateapipb.ObjectRef{Atespace: "team-a", Name: "sub-tmpl"}}
		got, err := resolveActorTemplate(ctx, persistence, actor)
		if err != nil {
			t.Fatalf("resolveActorTemplate: %v", err)
		}
		if got.GetMetadata().GetUid() != stored.GetMetadata().GetUid() {
			t.Errorf("template uid = %q, want the stored substrate template %q", got.GetMetadata().GetUid(), stored.GetMetadata().GetUid())
		}
	})

	t.Run("ref to a missing template is FailedPrecondition", func(t *testing.T) {
		actor := &ateapipb.Actor{ActorTemplate: &ateapipb.ObjectRef{Atespace: "team-a", Name: "absent"}}
		_, err := resolveActorTemplate(ctx, persistence, actor)
		if got := status.Code(err); got != codes.FailedPrecondition {
			t.Fatalf("status.Code = %v, want FailedPrecondition (err: %v)", got, err)
		}
	})
}

// TestResolveActorTemplate_NotFound verifies a vanished template and an actor
// naming no template at all surface errActorTemplateNotFound, so callers like
// delete can tolerate them.
func TestResolveActorTemplate_NotFound(t *testing.T) {
	ctx := context.Background()
	persistence := newTestPersistence(t)
	stored := seedSubstrateTemplate(t, ctx, persistence, "sub-tmpl")

	tests := []struct {
		name         string
		actor        *ateapipb.Actor
		wantNotFound bool
	}{
		{"ref resolves", &ateapipb.Actor{ActorTemplate: &ateapipb.ObjectRef{Atespace: "team-a", Name: "sub-tmpl"}}, false},
		{"ref to deleted template", &ateapipb.Actor{ActorTemplate: &ateapipb.ObjectRef{Atespace: "team-a", Name: "gone"}}, true},
		{"no template named at all", &ateapipb.Actor{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveActorTemplate(ctx, persistence, tc.actor)
			if tc.wantNotFound {
				if !errors.Is(err, errActorTemplateNotFound) {
					t.Fatalf("resolveActorTemplate err = %v, want errActorTemplateNotFound", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveActorTemplate: %v", err)
			}
			if got.GetMetadata().GetUid() != stored.GetMetadata().GetUid() {
				t.Errorf("template uid = %q, want %q", got.GetMetadata().GetUid(), stored.GetMetadata().GetUid())
			}
		})
	}
}

// TestActorTemplateObjectRef pins that snapshot and assignment records get a
// fresh copy of the reference, never the actor's own message.
func TestActorTemplateObjectRef(t *testing.T) {
	if got := actorTemplateObjectRef(&ateapipb.Actor{}); got != nil {
		t.Errorf("actorTemplateObjectRef(no ref) = %v, want nil", got)
	}
	actor := &ateapipb.Actor{ActorTemplate: &ateapipb.ObjectRef{Atespace: "team-a", Name: "tmpl1"}}
	got := actorTemplateObjectRef(actor)
	if got == actor.GetActorTemplate() {
		t.Error("actorTemplateObjectRef aliases the actor's reference")
	}
	if got.GetAtespace() != "team-a" || got.GetName() != "tmpl1" {
		t.Errorf("actorTemplateObjectRef = %v, want team-a/tmpl1", got)
	}
}
