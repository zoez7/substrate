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

// Package atemanifest parses manifest files holding substrate API resources
// in their protojson form, as written by the demos and accepted by
// `kubectl ate create actor-template -f`.
package atemanifest

import (
	"fmt"

	"google.golang.org/protobuf/encoding/protojson"
	"sigs.k8s.io/yaml"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

// ParseActorTemplate parses a single protojson-shaped YAML or JSON document
// into an ActorTemplate. Parsing is strict: unknown fields are an error, so
// typos don't silently drop configuration.
func ParseActorTemplate(data []byte) (*ateapipb.ActorTemplate, error) {
	jsonData, err := yaml.YAMLToJSON(data)
	if err != nil {
		return nil, fmt.Errorf("invalid YAML: %w", err)
	}
	if string(jsonData) == "null" {
		return nil, fmt.Errorf("manifest is empty")
	}
	template := &ateapipb.ActorTemplate{}
	if err := protojson.Unmarshal(jsonData, template); err != nil {
		return nil, err
	}
	return template, nil
}
