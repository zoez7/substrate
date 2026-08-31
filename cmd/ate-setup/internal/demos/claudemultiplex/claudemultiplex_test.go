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

package claudemultiplex

import (
	"testing"

	"github.com/agent-substrate/substrate/cmd/ate-setup/internal/demos"
	"github.com/agent-substrate/substrate/cmd/ate-setup/internal/demos/demotest"
)

// TestPoolRendersWithoutCredentials covers the delete-time rendering of the
// pool manifest, which DeleteAll relies on producing valid YAML even with no
// credentials in the environment.
func TestPoolRendersWithoutCredentials(t *testing.T) {
	e := demotest.Env(t)
	e.Cfg.BucketName = ""

	manifest, err := demos.Render(e, poolTemplate, nil, nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	demotest.AssertRendered(t, manifest)
}

// TestAgentTemplatesParse renders each agent's substrate template with the
// deploy-time placeholder values and parses it under the strict protojson
// rules `create actor-template` applies. The demo has its own placeholders,
// so the sweep test in the demos package does not cover it.
func TestAgentTemplatesParse(t *testing.T) {
	e := demotest.Env(t)

	for _, agent := range agents {
		t.Run(agent.Name, func(t *testing.T) {
			manifest, err := demos.Render(e, templateManifest(agent), map[string]string{
				"ANTHROPIC_API_KEY": "test-key",
				"WORKLOAD_IMAGE":    "registry.example/workload@sha256:0000000000000000000000000000000000000000000000000000000000000000",
			}, nil)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			demotest.AssertActorTemplateManifest(t, manifest, agent)
		})
	}
}
