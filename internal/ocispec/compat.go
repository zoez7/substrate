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

package ocispec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/opencontainers/runtime-spec/specs-go"
	"go.ate.dev/substrate/internal/proto/ateworkerpb"
	"go.ate.dev/substrate/internal/sizing"
)

func probeActorDirs() *ateworkerpb.ActorDirs {
	return &ateworkerpb.ActorDirs{
		OciBundleDir:              "/probe/bundles",
		DurableDirVolumeMountsDir: "/probe/durable-dir",
		ExternalVolumeMountsDir:   "/probe/volumes",
		SystemInfoVolumeRootsDir:  "/probe/system-info",
	}
}

func probeContainer() *ateworkerpb.Container {
	return &ateworkerpb.Container{
		Name: "probe",
		ContainerSpec: &ateworkerpb.ContainerSpec{
			Args:         []string{"/probe"},
			Env:          []string{"PROBE=1"},
			Cwd:          "/work",
			User:         &ateworkerpb.ProcessUser{Uid: 1000, Gid: 1000, AdditionalGids: []uint32{2000}},
			Capabilities: []string{"CAP_NET_BIND_SERVICE"},
			Resources: &ateworkerpb.ResourceRequirements{
				Limits: &ateworkerpb.Resources{
					CpuMillicores: 1000,
					MemoryBytes:   512 * 1024 * 1024,
				},
			},
		},
		DurableDirVolumeMounts: []*ateworkerpb.VolumeMount{{Name: "vol", MountPath: "/mnt/durable"}},
		ExternalVolumeMounts:   []*ateworkerpb.VolumeMount{{Name: "ext", MountPath: "/mnt/ext"}},
		SystemInfoVolumeMounts: []*ateworkerpb.VolumeMount{{Name: "sys", MountPath: "/mnt/sys"}},
		ImageVolumeMounts:      []*ateworkerpb.VolumeMount{{Name: "img", MountPath: "/mnt/img"}},
	}
}

func buildGVisorProbeSpecs() (sandbox, container *specs.Spec) {
	dirs := probeActorDirs()
	c := probeContainer()

	sandbox = BuildSandbox("probe-uid")
	ShapeGVisor(sandbox, GVisorOptions{
		ActorUID:       "probe-uid",
		ActorDirs:      dirs,
		ContainerName:  SandboxID,
		DurableVolumes: []string{"vol"},
		Size:           sizing.SandboxSize{MilliCPU: 1000, MemoryBytes: 512 * 1024 * 1024},
	})

	container = Build(Options{
		ActorUID:  "probe-uid",
		ActorDirs: dirs,
		Container: c,
	})
	ShapeGVisor(container, GVisorOptions{
		ActorUID:      "probe-uid",
		ActorDirs:     dirs,
		ContainerName: c.GetName(),
		Size:          sizing.SandboxSize{MilliCPU: 1000, MemoryBytes: 512 * 1024 * 1024},
	})
	return sandbox, container
}

func buildMicroVMProbeSpec() (*specs.Spec, error) {
	dirs := probeActorDirs()
	c := probeContainer()
	spec := Build(Options{
		ActorUID:  "probe-uid",
		ActorDirs: dirs,
		Container: c,
	})
	if err := ShapeMicroVM(spec, MicroVMOptions{
		ActorDirs:     dirs,
		ContainerName: c.GetName(),
	}); err != nil {
		return nil, err
	}
	return spec, nil
}

func hashSpecs(specs ...*specs.Spec) string {
	h := sha256.New()
	for _, s := range specs {
		b, err := json.Marshal(s)
		if err != nil {
			panic(fmt.Sprintf("ocispec: marshal probe spec: %v", err))
		}
		h.Write(b)
		h.Write([]byte{'\n'})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// GVisorSpecCompatHash returns a deterministic digest of the OCI sandbox and
// container specs produced by BuildSandbox, Build, and ShapeGVisor. Any change
// to the shaped spec structure changes the digest.
func GVisorSpecCompatHash() string {
	sandbox, container := buildGVisorProbeSpecs()
	return hashSpecs(sandbox, container)
}

// MicroVMSpecCompatHash returns a deterministic digest of the OCI container
// spec produced by Build and ShapeMicroVM. Any change to the shaped spec
// structure changes the digest.
func MicroVMSpecCompatHash() string {
	spec, err := buildMicroVMProbeSpec()
	if err != nil {
		panic(fmt.Sprintf("ocispec: shape microvm probe spec: %v", err))
	}
	return hashSpecs(spec)
}
