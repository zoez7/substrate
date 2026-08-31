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

package counter

import (
	"testing"

	"github.com/agent-substrate/substrate/cmd/ate-setup/internal/demos"
	"github.com/agent-substrate/substrate/cmd/ate-setup/internal/demos/demotest"
	"github.com/agent-substrate/substrate/internal/atemanifest"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

// TestExternalVolumeRenders covers the substitution branch of the counter
// template, where the external-volume placeholders carry multi-line values
// instead of being dropped. The drop branch is covered by the sweep test in
// the demos package. Parsing the result as strict protojson is the guard
// that the injected stanzas land at the right indentation: misplaced YAML
// would surface as an unknown field or a missing volume here rather than at
// deploy time.
func TestExternalVolumeRenders(t *testing.T) {
	e := demotest.Env(t)

	manifest, err := demos.RenderTemplateManifest(e, templateManifest, externalVolumeValues(e))
	if err != nil {
		t.Fatalf("RenderTemplateManifest: %v", err)
	}

	tmpl, err := atemanifest.ParseActorTemplate(manifest)
	if err != nil {
		t.Fatalf("ParseActorTemplate: %v\nmanifest:\n%s", err, manifest)
	}

	var external *ateapipb.Volume
	for _, v := range tmpl.GetVolumes() {
		if v.GetName() == "external-data" {
			external = v
		}
	}
	if external == nil {
		t.Fatalf("rendered template has no external-data volume: %v", tmpl.GetVolumes())
	}
	if external.GetType() != "ExternalVolumeTemplate" {
		t.Errorf("external-data volume type = %q, want ExternalVolumeTemplate", external.GetType())
	}
	if got := external.GetExternalVolumeTemplate().GetStorageClassName(); got != "standard" {
		t.Errorf("storageClassName = %q, want standard", got)
	}

	container := tmpl.GetContainers()[0]
	var hasArg, hasMount bool
	for _, arg := range container.GetCommand() {
		if arg == "--validate-existing-file-path=/external-data/test.txt" {
			hasArg = true
		}
	}
	for _, m := range container.GetVolumeMounts() {
		if m.GetName() == "external-data" && m.GetMountPath() == "/external-data" {
			hasMount = true
		}
	}
	if !hasArg {
		t.Errorf("container command %v is missing the validation flag", container.GetCommand())
	}
	if !hasMount {
		t.Errorf("container volumeMounts %v are missing external-data", container.GetVolumeMounts())
	}
}
