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
	"strings"
	"testing"

	"github.com/opencontainers/runtime-spec/specs-go"
)

func TestSpecCompatHash_DeterministicAndFormatted(t *testing.T) {
	gv1 := GVisorSpecCompatHash()
	gv2 := GVisorSpecCompatHash()
	if gv1 != gv2 {
		t.Fatalf("GVisorSpecCompatHash not deterministic: %q vs %q", gv1, gv2)
	}
	if !strings.HasPrefix(gv1, "sha256:") || len(gv1) != len("sha256:")+64 {
		t.Errorf("GVisorSpecCompatHash = %q, want sha256:<64 hex chars>", gv1)
	}

	vm1 := MicroVMSpecCompatHash()
	vm2 := MicroVMSpecCompatHash()
	if vm1 != vm2 {
		t.Fatalf("MicroVMSpecCompatHash not deterministic: %q vs %q", vm1, vm2)
	}
	if !strings.HasPrefix(vm1, "sha256:") || len(vm1) != len("sha256:")+64 {
		t.Errorf("MicroVMSpecCompatHash = %q, want sha256:<64 hex chars>", vm1)
	}

	if gv1 == vm1 {
		t.Errorf("gVisor and microVM compat hashes should differ, both = %q", gv1)
	}
}

func TestGVisorSpecCompatHash_SensitiveToSpecChanges(t *testing.T) {
	base := GVisorSpecCompatHash()

	for _, tc := range []struct {
		name   string
		mutate func(sandbox, container *specs.Spec)
	}{
		{
			name: "sandbox annotation",
			mutate: func(sandbox, _ *specs.Spec) {
				sandbox.Annotations["dev.gvisor.spec.mount.vol.share"] = "pod"
			},
		},
		{
			name: "container rlimit",
			mutate: func(_, container *specs.Spec) {
				container.Process.Rlimits[0].Hard = 4096
			},
		},
		{
			name: "container mount options",
			mutate: func(_, container *specs.Spec) {
				container.Mounts[0].Options = append(container.Mounts[0].Options, "nosuid")
			},
		},
		{
			name: "hostname",
			mutate: func(_, container *specs.Spec) {
				container.Hostname = "other"
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, c := buildGVisorProbeSpecs()
			tc.mutate(s, c)
			if got := hashSpecs(s, c); got == base {
				t.Errorf("hash did not change after mutating %s", tc.name)
			}
		})
	}
}

func TestMicroVMSpecCompatHash_SensitiveToSpecChanges(t *testing.T) {
	base := MicroVMSpecCompatHash()

	for _, tc := range []struct {
		name   string
		mutate func(spec *specs.Spec)
	}{
		{
			name: "mount options",
			mutate: func(spec *specs.Spec) {
				spec.Mounts[0].Options = append(spec.Mounts[0].Options, "ro")
			},
		},
		{
			name: "sysctl",
			mutate: func(spec *specs.Spec) {
				spec.Linux.Sysctl["net.ipv4.ip_unprivileged_port_start"] = "1024"
			},
		},
		{
			name: "masked paths",
			mutate: func(spec *specs.Spec) {
				spec.Linux.MaskedPaths = append(spec.Linux.MaskedPaths, "/proc/extra")
			},
		},
		{
			name: "readonly paths",
			mutate: func(spec *specs.Spec) {
				spec.Linux.ReadonlyPaths = append(spec.Linux.ReadonlyPaths, "/proc/extra")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := buildMicroVMProbeSpec()
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(s)
			if got := hashSpecs(s); got == base {
				t.Errorf("hash did not change after mutating %s", tc.name)
			}
		})
	}
}
