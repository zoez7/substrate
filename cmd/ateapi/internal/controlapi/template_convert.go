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
	"fmt"

	"github.com/agent-substrate/substrate/cmd/ateapi/internal/store"
	"github.com/agent-substrate/substrate/internal/resources"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// actorTemplateGetter is the storage subset template resolution needs.
type actorTemplateGetter interface {
	GetActorTemplate(ctx context.Context, templateRef resources.ActorTemplateRef) (*ateapipb.ActorTemplate, error)
}

// errActorTemplateNotFound matches (via errors.Is) resolution failures where
// the actor names a template that does not exist. Most callers return the
// error as is — it already carries FailedPrecondition — while delete
// tolerates it and cleans up without the template.
var errActorTemplateNotFound = status.New(codes.FailedPrecondition, "actor template not found").Err()

// resolveActorTemplate resolves the substrate ActorTemplate the actor's
// actor_template ref names. A missing template surfaces as
// errActorTemplateNotFound.
func resolveActorTemplate(ctx context.Context, st actorTemplateGetter, actor *ateapipb.Actor) (*ateapipb.ActorTemplate, error) {
	templateRef := resources.ActorTemplateRefFromObjectRef(actor.GetActorTemplate())
	template, err := st.GetActorTemplate(ctx, templateRef)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("%w; ObjectRef: %s ", errActorTemplateNotFound, templateRef)
	}
	if err != nil {
		return nil, fmt.Errorf("while getting ActorTemplate: %w", err)
	}
	return template, nil
}

// actorTemplateObjectRef returns a fresh copy of the actor's template
// reference — fresh so records built from it never alias the actor message.
func actorTemplateObjectRef(actor *ateapipb.Actor) *ateapipb.ObjectRef {
	ref := actor.GetActorTemplate()
	if ref == nil {
		return nil
	}
	return &ateapipb.ObjectRef{Atespace: ref.GetAtespace(), Name: ref.GetName()}
}
