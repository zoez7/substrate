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

package atemanifest

import (
	"testing"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"
)

const counterTemplateManifest = `metadata:
  atespace: ate-demo-counter
  name: counter
workerSelector:
  matchLabels:
    workload: counter
containers:
- name: counter
  image: ko://github.com/agent-substrate/substrate/demos/counter
  command: ["/ko-app/counter", "--extra-port=9090"]
  readyz:
    httpGet:
      path: /readyz
      port: 80
  volumeMounts:
  - name: data
    mountPath: /home/counter
resources:
  limits:
  - name: cpu
    quantity: "1"
  - name: memory
    quantity: 512Mi
snapshotsConfig:
  onPause: SNAPSHOT_CONTENT_SCOPE_FULL
  onCommit: SNAPSHOT_CONTENT_SCOPE_FULL
  storageLocation: gs://ate-snapshots/ate-demo-counter/
sandboxConfig:
  sandboxClass: SANDBOX_CLASS_GVISOR
  configName: gvisor-default
volumes:
- name: data
  type: DurableDir
  durableDir: {}
`

func TestParseActorTemplate(t *testing.T) {
	got, err := ParseActorTemplate([]byte(counterTemplateManifest))
	if err != nil {
		t.Fatalf("ParseActorTemplate: %v", err)
	}

	want := &ateapipb.ActorTemplate{
		Metadata:       &ateapipb.ResourceMetadata{Atespace: "ate-demo-counter", Name: "counter"},
		WorkerSelector: &ateapipb.Selector{MatchLabels: map[string]string{"workload": "counter"}},
		Containers: []*ateapipb.Container{{
			Name:    "counter",
			Image:   "ko://github.com/agent-substrate/substrate/demos/counter",
			Command: []string{"/ko-app/counter", "--extra-port=9090"},
			Readyz: &ateapipb.ContainerReadyz{
				HttpGet: &ateapipb.HTTPGetAction{Path: "/readyz", Port: 80},
			},
			VolumeMounts: []*ateapipb.VolumeMount{{Name: "data", MountPath: "/home/counter"}},
		}},
		Resources: &ateapipb.Resources{Limits: []*ateapipb.Limits{
			{Name: "cpu", Quantity: "1"},
			{Name: "memory", Quantity: "512Mi"},
		}},
		SnapshotsConfig: &ateapipb.SnapshotsConfig{
			OnPause:         ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL,
			OnCommit:        ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL,
			StorageLocation: "gs://ate-snapshots/ate-demo-counter/",
		},
		SandboxConfig: &ateapipb.SandboxConfig{
			SandboxClass: ateapipb.SandboxClass_SANDBOX_CLASS_GVISOR,
			ConfigName:   "gvisor-default",
		},
		Volumes: []*ateapipb.Volume{{
			Name:       "data",
			Type:       "DurableDir",
			DurableDir: &ateapipb.DurableDirVolumeSource{},
		}},
	}
	if diff := cmp.Diff(want, got, protocmp.Transform()); diff != "" {
		t.Errorf("template mismatch (-want +got):\n%s", diff)
	}
}

func TestParseActorTemplate_SnakeCase(t *testing.T) {
	// protojson accepts the proto field names as well as the json names.
	manifest := `metadata:
  atespace: ate-demo-counter
  name: counter
snapshots_config:
  on_pause: SNAPSHOT_CONTENT_SCOPE_FULL
  storage_location: gs://ate-snapshots/ate-demo-counter/
sandbox_config:
  sandbox_class: SANDBOX_CLASS_MICROVM
  config_name: microvm
`
	got, err := ParseActorTemplate([]byte(manifest))
	if err != nil {
		t.Fatalf("ParseActorTemplate: %v", err)
	}
	if got.GetSnapshotsConfig().GetStorageLocation() != "gs://ate-snapshots/ate-demo-counter/" {
		t.Errorf("storage_location = %q", got.GetSnapshotsConfig().GetStorageLocation())
	}
	if got.GetSandboxConfig().GetSandboxClass() != ateapipb.SandboxClass_SANDBOX_CLASS_MICROVM {
		t.Errorf("sandbox_class = %v", got.GetSandboxConfig().GetSandboxClass())
	}
}

func TestParseActorTemplate_Errors(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
	}{
		{name: "empty", manifest: ""},
		{name: "unknown field", manifest: "metadata: {atespace: a, name: n}\nsandboxClass: gvisor\n"},
		{name: "bad enum", manifest: "sandboxConfig: {sandboxClass: gvisor}\n"},
		{name: "crd shape", manifest: "apiVersion: ate.dev/v1alpha1\nkind: ActorTemplate\nmetadata: {name: counter}\n"},
		{name: "not yaml", manifest: "\t{"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got, err := ParseActorTemplate([]byte(test.manifest)); err == nil {
				t.Fatalf("ParseActorTemplate succeeded: %v", got)
			}
		})
	}
}
