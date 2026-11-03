//go:build linux

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

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/runtime-spec/specs-go"
	"google.golang.org/grpc/codes"

	"go.ate.dev/substrate/internal/actorlock"
	"go.ate.dev/substrate/internal/actorlog"
	"go.ate.dev/substrate/internal/apierror"
	"go.ate.dev/substrate/internal/hardware"
	"go.ate.dev/substrate/internal/ocispec"
	"go.ate.dev/substrate/internal/proto/ateletpb"
	"go.ate.dev/substrate/internal/proto/ateworkerpb"
	"go.ate.dev/substrate/internal/resources"
)

var testActorDirs = &ateworkerpb.ActorDirs{
	RootDir:      "/node/actors/test-actor-123",
	OciBundleDir: "/node/actors/test-actor-123/bundle",
}

// writeSpec builds a workload container's spec from the request, and a
// sandbox spec with no process and no rootfs. It makes the sandbox bundle.
func TestWriteSpec(t *testing.T) {
	root := t.TempDir()
	actorDirs := &ateworkerpb.ActorDirs{RootDir: root, OciBundleDir: filepath.Join(root, "bundles")}
	r := &runsc{
		actorUID:   "uid-a",
		actorDirs:  actorDirs,
		containers: []*ateworkerpb.Container{{Name: "app", ContainerSpec: &ateworkerpb.ContainerSpec{Args: []string{"/app", "serve"}}}},
	}
	// atelet makes the application bundle; writeSpec does not make it.
	if err := r.writeSpec("app"); err == nil {
		t.Error("writeSpec(app) = nil without the bundle atelet makes")
	}
	if err := os.MkdirAll(ociBundlePath(actorDirs, "app"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"app", ocispec.SandboxID} {
		if err := r.writeSpec(name); err != nil {
			t.Fatalf("writeSpec(%q) = %v", name, err)
		}
		b, err := os.ReadFile(filepath.Join(ociBundlePath(actorDirs, name), "config.json"))
		if err != nil {
			t.Fatal(err)
		}
		var spec specs.Spec
		if err := json.Unmarshal(b, &spec); err != nil {
			t.Fatal(err)
		}
		if got := spec.Annotations["io.kubernetes.cri.container-name"]; got != name {
			t.Errorf("%s container-name annotation = %q, want the spec shaped for gVisor", name, got)
		}
		if name == ocispec.SandboxID {
			if spec.Process != nil || spec.Root != nil || spec.Mounts != nil {
				t.Errorf("sandbox spec has process %v, root %v, mounts %v; want none", spec.Process, spec.Root, spec.Mounts)
			}
			continue
		}
		if want := []string{"/app", "serve"}; spec.Process == nil || !slices.Equal(spec.Process.Args, want) {
			t.Errorf("%s process = %+v, want args %v", name, spec.Process, want)
		}
	}
	if err := r.writeSpec("missing"); err == nil {
		t.Error("writeSpec() = nil for a container outside the workload spec")
	}
}

func mustFlagStore(t *testing.T) *flagStore {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "flags.json")
	const testFlagsJSON = `{
  "globalFlags": [
    "-global1",
    "-global2"
  ],
  "commands": {
    "restore": {
      "globalFlags": ["-restore-global"],
      "subcommandFlags": ["-restore-sub1", "-restore-sub2"]
    },
    "start": {
      "globalFlags": ["-start-global"]
    },
    "list": {
      "subcommandFlags": ["-list-sub"]
    }
  }
}`
	if err := os.WriteFile(cfgPath, []byte(testFlagsJSON), 0o600); err != nil {
		t.Fatalf("failed to write test flags.json: %v", err)
	}
	store, err := newFlagStore(t.Context(), cfgPath)
	if err != nil {
		t.Fatalf("newFlagStore(%s) failed: %v", cfgPath, err)
	}
	return store
}

func TestBuildArgs(t *testing.T) {
	r := &runsc{
		path:      "/usr/bin/runsc",
		actorUID:  "test-actor-123",
		actorDirs: testActorDirs,
		flags:     mustFlagStore(t),
	}

	for _, tc := range []struct {
		name          string
		command       string
		containerName string
		extraArgs     []string
		want          []string
	}{
		{
			name:          "kill",
			command:       "kill",
			containerName: "",
			extraArgs:     []string{"my-container", "SIGTERM"},
			want: []string{
				"-global1",
				"-global2",
				"-root", "/node/actors/test-actor-123/runsc-state",
				"kill",
				"my-container",
				"SIGTERM",
			},
		},
		{
			name:          "wait",
			command:       "wait",
			containerName: "my-container",
			want: []string{
				"-global1",
				"-global2",
				"-root", "/node/actors/test-actor-123/runsc-state",
				"wait",
				"my-container",
			},
		},
		{
			name:          "wait -restore",
			command:       "wait",
			containerName: "my-container",
			extraArgs:     []string{"-restore"},
			want: []string{
				"-global1",
				"-global2",
				"-root", "/node/actors/test-actor-123/runsc-state",
				"wait",
				"-restore",
				"my-container",
			},
		},
		{
			name:          "wait -fsrestore",
			command:       "wait",
			containerName: "my-container",
			extraArgs:     []string{"-fsrestore"},
			want: []string{
				"-global1",
				"-global2",
				"-root", "/node/actors/test-actor-123/runsc-state",
				"wait",
				"-fsrestore",
				"my-container",
			},
		},
		{
			name:          "list",
			command:       "list",
			containerName: "",
			want: []string{
				"-global1",
				"-global2",
				"-root", "/node/actors/test-actor-123/runsc-state",
				"list",
				"-list-sub",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := r.buildArgs(tc.command, tc.containerName, tc.extraArgs...)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("buildArgs(%q, %q, %v) = %v, want %v", tc.command, tc.containerName, tc.extraArgs, got, tc.want)
			}
		})
	}
}

func TestBuildArgsEnableCoreTags(t *testing.T) {
	r := &runsc{
		path:           "/usr/bin/runsc",
		actorUID:       "test-actor-123",
		actorDirs:      testActorDirs,
		flags:          mustFlagStore(t),
		enableCoreTags: true,
	}

	got := r.buildArgs("wait", "my-container")
	want := []string{
		"-global1",
		"-global2",
		"--enable-core-tags",
		"-root", "/node/actors/test-actor-123/runsc-state",
		"wait",
		"my-container",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildArgs(\"wait\", \"my-container\") = %v, want %v", got, want)
	}
}

func TestCheckpointArgs(t *testing.T) {
	r := &runsc{
		path:      "/usr/bin/runsc",
		actorUID:  "test-actor-123",
		actorDirs: testActorDirs,
		flags:     mustFlagStore(t),
	}
	checkpointDir := "/node/actors/test-actor-123/checkpoints/snap-1"

	got := r.checkpointArgs(ocispec.SandboxID, checkpointDir, []string{"rootfs/=app:/", "fs/=app:/var/data"})
	want := []string{
		"-global1",
		"-global2",
		"-root", "/node/actors/test-actor-123/runsc-state",
		"checkpoint",
		"-image-path", checkpointDir,
		"-fs-checkpoint-paths", "rootfs/=app:/",
		"-fs-checkpoint-paths", "fs/=app:/var/data",
		ocispec.SandboxID,
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("checkpointArgs() = %v, want %v", got, want)
	}
}

func TestFsCheckpointArgs(t *testing.T) {
	r := &runsc{
		path:      "/usr/bin/runsc",
		actorUID:  "test-actor-123",
		actorDirs: testActorDirs,
		flags:     mustFlagStore(t),
	}
	checkpointDir := "/node/actors/test-actor-123/checkpoints/snap-1"

	got := r.fsCheckpointArgs(ocispec.SandboxID, checkpointDir, []string{"rootfs/=app:/", "fs/=app:/var/data"})
	want := []string{
		"-global1",
		"-global2",
		"-root", "/node/actors/test-actor-123/runsc-state",
		"fscheckpoint",
		"-image-path", checkpointDir,
		"-path", "rootfs/=app:/",
		"-path", "fs/=app:/var/data",
		ocispec.SandboxID,
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("fsCheckpointArgs() = %v, want %v", got, want)
	}
}

// Only the sandbox gets -no-root-container: runsc refuses it for any other
// container.
func TestCreateArgs(t *testing.T) {
	r := &runsc{
		path:      "/usr/bin/runsc",
		actorUID:  "test-actor-123",
		actorDirs: testActorDirs,
		flags:     mustFlagStore(t),
	}
	for _, tc := range []struct {
		container string
		extra     []string
		want      []string
	}{{
		container: ocispec.SandboxID,
		extra:     []string{"-fs-restore-image-path", "/snap/fs"},
		want: []string{
			"-global1",
			"-global2",
			"-root", "/node/actors/test-actor-123/runsc-state",
			"create",
			"-no-root-container",
			"-bundle", "/node/actors/test-actor-123/bundle/" + ocispec.SandboxID,
			"-pid-file", "/node/actors/test-actor-123/pidfiles/" + ocispec.SandboxID + ".pid",
			"-fs-restore-image-path", "/snap/fs",
			ocispec.SandboxID,
		},
	}, {
		container: "app",
		want: []string{
			"-global1",
			"-global2",
			"-root", "/node/actors/test-actor-123/runsc-state",
			"create",
			"-bundle", "/node/actors/test-actor-123/bundle/app",
			"-pid-file", "/node/actors/test-actor-123/pidfiles/app.pid",
			"app",
		},
	}} {
		if got := r.createArgs(tc.container, tc.extra); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("createArgs(%q) = %v, want %v", tc.container, got, tc.want)
		}
	}
}

func TestRestoreArgs(t *testing.T) {
	r := &runsc{
		path:      "/usr/bin/runsc",
		actorUID:  "test-actor-123",
		actorDirs: testActorDirs,
		flags:     mustFlagStore(t),
	}
	checkpointDir := "/node/actors/test-actor-123/checkpoints/snap-1"

	for _, tc := range []struct {
		container string
		want      []string
	}{{
		container: ocispec.SandboxID,
		want: []string{
			"-global1",
			"-global2",
			"-restore-global",
			"-root", "/node/actors/test-actor-123/runsc-state",
			"restore",
			"-restore-sub1",
			"-restore-sub2",
			"-no-root-container",
			"-bundle", "/node/actors/test-actor-123/bundle/" + ocispec.SandboxID,
			"-image-path", checkpointDir,
			"-pid-file", "/node/actors/test-actor-123/pidfiles/" + ocispec.SandboxID + ".pid",
			ocispec.SandboxID,
		},
	}, {
		container: "app",
		want: []string{
			"-global1",
			"-global2",
			"-restore-global",
			"-root", "/node/actors/test-actor-123/runsc-state",
			"restore",
			"-restore-sub1",
			"-restore-sub2",
			"-bundle", "/node/actors/test-actor-123/bundle/app",
			"-image-path", checkpointDir,
			"-pid-file", "/node/actors/test-actor-123/pidfiles/app.pid",
			"app",
		},
	}} {
		if got := r.restoreArgs(tc.container, checkpointDir); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("restoreArgs(%q) = %v, want %v", tc.container, got, tc.want)
		}
	}
}

func TestRestoreWorkloadRejectsEmptyRestoreDir(t *testing.T) {
	s := &AteWorkerService{
		locks:    actorlock.New(),
		inFlight: actorlock.NewInFlight(),
	}

	noDirs := validRestoreRequest()
	noDirs.ActorDirs = nil
	noRestoreDir := validRestoreRequest()
	noRestoreDir.ActorDirs.RestoreDir = ""
	for _, tc := range []struct {
		name string
		req  *ateworkerpb.RestoreWorkloadRequest
		want string
	}{
		{name: "nil actor_dirs", req: noDirs, want: "actor_dirs"},
		{name: "empty restore_dir", req: noRestoreDir, want: "restore_dir"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.RestoreWorkload(context.Background(), tc.req)
			if apierror.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("RestoreWorkload() = %v, want InvalidArgument naming %s", err, tc.want)
			}
		})
	}
}

func TestGVisorRuntimeAttributes(t *testing.T) {
	attrs := gvisorRuntimeAttributes()
	byKey := make(map[string]*ateletpb.AttributeEntry, len(attrs))
	for _, a := range attrs {
		byKey[a.GetKey()] = a
		if got := a.GetAcceptedValues(); len(got) != 1 || got[0].GetValue() != "" || got[0].GetFidelity() != ateletpb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY {
			t.Errorf("%s accepted_values = %v, want [{value: \"\", fidelity: MEMORY}]", a.GetKey(), got)
		}
	}
	if got := byKey[hardware.AttrMemoryCompat].GetValue(); !strings.HasPrefix(got, "sha256:") || len(got) != len("sha256:")+64 {
		t.Errorf("memory_compat = %q, want sha256:<64 hex chars>", got)
	}
	if got := byKey[hardware.AttrRootfsCompat].GetValue(); got != "v1" {
		t.Errorf("rootfs_compat = %q, want v1", got)
	}
	if got := byKey[hardware.AttrVolumesCompat].GetValue(); got != "v1" {
		t.Errorf("volumes_compat = %q, want v1", got)
	}
}

type fakeRestoreRuntime struct {
	calls     []string
	onCreate  func(containerName string, extraArgs []string) error
	onStart   func(containerName string) error
	onRestore func(containerName, checkpointPath string) error
	onDelete  func(containerName string) error
}

func (f *fakeRestoreRuntime) cmdCreate(_ context.Context, _ io.Writer, containerName string, extraArgs []string) error {
	call := "create " + containerName
	if len(extraArgs) > 0 {
		call += " " + strings.Join(extraArgs, " ")
	}
	f.calls = append(f.calls, call)
	if f.onCreate != nil {
		return f.onCreate(containerName, extraArgs)
	}
	return nil
}

func (f *fakeRestoreRuntime) cmdStart(_ context.Context, _ io.Writer, containerName string) error {
	f.calls = append(f.calls, "start "+containerName)
	if f.onStart != nil {
		return f.onStart(containerName)
	}
	return nil
}

func (f *fakeRestoreRuntime) cmdRestore(_ context.Context, _ io.Writer, containerName, checkpointPath string) error {
	f.calls = append(f.calls, "restore "+containerName)
	if f.onRestore != nil {
		return f.onRestore(containerName, checkpointPath)
	}
	return nil
}

func (f *fakeRestoreRuntime) cmdDelete(_ context.Context, containerName string) error {
	f.calls = append(f.calls, "delete "+containerName)
	if f.onDelete != nil {
		return f.onDelete(containerName)
	}
	return nil
}

func setupRestoreTestDirs(t *testing.T, withRootfsCheckpoint, withFSCheckpoint bool) (*AteWorkerService, *hostedActor, *ateworkerpb.ActorDirs, []*ateworkerpb.Container) {
	t.Helper()
	root := t.TempDir()
	bundleDir := filepath.Join(root, "bundles")
	restoreDir := filepath.Join(root, "restore")
	if err := os.MkdirAll(filepath.Join(bundleDir, "app", "rootfs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(restoreDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if withRootfsCheckpoint {
		rootfsDir := filepath.Join(restoreDir, resources.RootfsCheckpointSubdir)
		if err := os.MkdirAll(rootfsDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rootfsDir, fsCheckpointManifest), []byte("manifest"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if withFSCheckpoint {
		fsDir := filepath.Join(restoreDir, resources.FSCheckpointSubdir)
		if err := os.MkdirAll(fsDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fsDir, fsCheckpointManifest), []byte("manifest"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	actorDirs := &ateworkerpb.ActorDirs{
		RootDir:      root,
		OciBundleDir: bundleDir,
		RestoreDir:   restoreDir,
	}
	if err := resetRunscStateAndPidFileDirs(actorDirs); err != nil {
		t.Fatal(err)
	}
	cgroupRoot := t.TempDir()
	s := &AteWorkerService{
		locks:       actorlock.New(),
		inFlight:    actorlock.NewInFlight(),
		actors:      map[string]*hostedActor{},
		actorLogger: actorlog.NewActorLogger(io.Discard, false),
		cgroupRoot:  cgroupRoot,
	}
	hosted := &hostedActor{
		attribution: resources.ActorAttribution{
			Ref: resources.ActorRef{Atespace: "team-a", Name: "act-1"},
			UID: "uid-1",
		},
	}
	containers := []*ateworkerpb.Container{{Name: "app"}}
	return s, hosted, actorDirs, containers
}

func TestRestoreWithFallback(t *testing.T) {
	t.Run("MEMORY failure cleans up and succeeds at ROOTFS", func(t *testing.T) {
		s, hosted, actorDirs, containers := setupRestoreTestDirs(t, true, true)
		rootfsDir := filepath.Join(actorDirs.GetRestoreDir(), resources.RootfsCheckpointSubdir)
		fsDir := filepath.Join(actorDirs.GetRestoreDir(), resources.FSCheckpointSubdir)

		// Seed a stale file in runsc-state and a sandbox cgroup leaf during the
		// MEMORY attempt to verify resetFailedRestoreAttempt clears both before
		// the ROOTFS attempt runs.
		staleStateFile := filepath.Join(runscStateDir(actorDirs), "stale-from-memory")
		cgroupLeaf := s.sandboxLeaf("uid-1")
		rt := &fakeRestoreRuntime{
			onCreate: func(containerName string, _ []string) error {
				if containerName == ocispec.SandboxID {
					if err := os.WriteFile(staleStateFile, []byte("state"), 0o600); err != nil {
						return err
					}
					if err := os.MkdirAll(cgroupLeaf, 0o700); err != nil {
						return err
					}
				}
				return nil
			},
			onRestore: func(containerName, _ string) error {
				if containerName == "app" {
					return errors.New("incompatible memory checkpoint")
				}
				return nil
			},
			onDelete: func(containerName string) error {
				// Leave a marker that only resetRunscStateAndPidFileDirs removes.
				if containerName == ocispec.SandboxID {
					return os.WriteFile(filepath.Join(runscStateDir(actorDirs), "leftover"), []byte("x"), 0o600)
				}
				return nil
			},
		}

		var containersToDelete []string
		var timing restoreTiming
		tLast := time.Now()
		gotFidelity, gotRestoreArgs, err := s.restoreWithFallback(
			context.Background(),
			rt,
			hosted,
			hosted.attribution,
			actorDirs,
			containers,
			ateworkerpb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY,
			&containersToDelete,
			&timing,
			&tLast,
		)
		if err != nil {
			t.Fatalf("restoreWithFallback() error = %v, want nil", err)
		}
		if gotFidelity != ateworkerpb.SnapshotFidelity_SNAPSHOT_FIDELITY_ROOTFS {
			t.Errorf("fidelity = %v, want %v", gotFidelity, ateworkerpb.SnapshotFidelity_SNAPSHOT_FIDELITY_ROOTFS)
		}
		wantArgs := []string{"--fs-restore-image-path", rootfsDir, "--fs-restore-image-path", fsDir}
		if !slices.Equal(gotRestoreArgs, wantArgs) {
			t.Errorf("restoreArgs = %v, want %v", gotRestoreArgs, wantArgs)
		}
		if _, err := os.Stat(filepath.Join(runscStateDir(actorDirs), "leftover")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("runsc-state leftover after reset = %v, want ErrNotExist", err)
		}
		bothArgs := fmt.Sprintf("create _sandbox --fs-restore-image-path %s --fs-restore-image-path %s", rootfsDir, fsDir)
		wantCalls := []string{
			bothArgs,
			"restore _sandbox",
			"create app",
			"restore app",
			"delete app",
			"delete _sandbox",
			bothArgs,
			"start _sandbox",
			"create app",
			"start app",
		}
		if !slices.Equal(rt.calls, wantCalls) {
			t.Errorf("calls = %v, want %v", rt.calls, wantCalls)
		}
		if !slices.Equal(containersToDelete, []string{ocispec.SandboxID, "app"}) {
			t.Errorf("containersToDelete = %v, want [_sandbox app]", containersToDelete)
		}
	})

	t.Run("missing rootfs fscheckpoint steps down from MEMORY through ROOTFS to VOLUMES", func(t *testing.T) {
		s, hosted, actorDirs, containers := setupRestoreTestDirs(t, false, true)
		fsDir := filepath.Join(actorDirs.GetRestoreDir(), resources.FSCheckpointSubdir)
		rt := &fakeRestoreRuntime{}

		var containersToDelete []string
		var timing restoreTiming
		tLast := time.Now()
		gotFidelity, gotRestoreArgs, err := s.restoreWithFallback(
			context.Background(),
			rt,
			hosted,
			hosted.attribution,
			actorDirs,
			containers,
			ateworkerpb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY,
			&containersToDelete,
			&timing,
			&tLast,
		)
		if err != nil {
			t.Fatalf("restoreWithFallback() error = %v, want nil", err)
		}
		if gotFidelity != ateworkerpb.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES {
			t.Errorf("fidelity = %v, want %v", gotFidelity, ateworkerpb.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES)
		}
		wantArgs := []string{"--fs-restore-image-path", fsDir}
		if !slices.Equal(gotRestoreArgs, wantArgs) {
			t.Errorf("restoreArgs = %v, want %v", gotRestoreArgs, wantArgs)
		}
		wantCalls := []string{
			fmt.Sprintf("create _sandbox --fs-restore-image-path %s", fsDir),
			"start _sandbox",
			"create app",
			"start app",
		}
		if !slices.Equal(rt.calls, wantCalls) {
			t.Errorf("calls = %v, want %v", rt.calls, wantCalls)
		}
	})

	t.Run("MEMORY and ROOTFS runtime failures step down to VOLUMES", func(t *testing.T) {
		s, hosted, actorDirs, containers := setupRestoreTestDirs(t, true, true)
		rootfsDir := filepath.Join(actorDirs.GetRestoreDir(), resources.RootfsCheckpointSubdir)
		fsDir := filepath.Join(actorDirs.GetRestoreDir(), resources.FSCheckpointSubdir)

		var startSandboxCalls int
		rt := &fakeRestoreRuntime{
			onRestore: func(containerName, _ string) error {
				return errors.New("memory restore failed")
			},
			onStart: func(containerName string) error {
				if containerName == ocispec.SandboxID {
					startSandboxCalls++
					if startSandboxCalls == 1 {
						return errors.New("rootfs restore failed")
					}
				}
				return nil
			},
		}

		var containersToDelete []string
		var timing restoreTiming
		tLast := time.Now()
		gotFidelity, gotRestoreArgs, err := s.restoreWithFallback(
			context.Background(),
			rt,
			hosted,
			hosted.attribution,
			actorDirs,
			containers,
			ateworkerpb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY,
			&containersToDelete,
			&timing,
			&tLast,
		)
		if err != nil {
			t.Fatalf("restoreWithFallback() error = %v, want nil", err)
		}
		if gotFidelity != ateworkerpb.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES {
			t.Errorf("fidelity = %v, want %v", gotFidelity, ateworkerpb.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES)
		}
		wantArgs := []string{"--fs-restore-image-path", fsDir}
		if !slices.Equal(gotRestoreArgs, wantArgs) {
			t.Errorf("restoreArgs = %v, want %v", gotRestoreArgs, wantArgs)
		}
		bothArgs := fmt.Sprintf("create _sandbox --fs-restore-image-path %s --fs-restore-image-path %s", rootfsDir, fsDir)
		volArgs := fmt.Sprintf("create _sandbox --fs-restore-image-path %s", fsDir)
		wantCalls := []string{
			bothArgs,
			"restore _sandbox",
			"delete _sandbox",
			bothArgs,
			"start _sandbox",
			"delete _sandbox",
			volArgs,
			"start _sandbox",
			"create app",
			"start app",
		}
		if !slices.Equal(rt.calls, wantCalls) {
			t.Errorf("calls = %v, want %v", rt.calls, wantCalls)
		}
	})

	t.Run("VOLUMES failure returns error and leaves containers for final cleanup", func(t *testing.T) {
		s, hosted, actorDirs, containers := setupRestoreTestDirs(t, true, true)
		rt := &fakeRestoreRuntime{
			onRestore: func(string, string) error {
				return errors.New("memory restore failed")
			},
			onStart: func(containerName string) error {
				if containerName == "app" {
					return errors.New("app start failed")
				}
				return nil
			},
		}

		var containersToDelete []string
		var timing restoreTiming
		tLast := time.Now()
		gotFidelity, _, err := s.restoreWithFallback(
			context.Background(),
			rt,
			hosted,
			hosted.attribution,
			actorDirs,
			containers,
			ateworkerpb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY,
			&containersToDelete,
			&timing,
			&tLast,
		)
		if err == nil || !strings.Contains(err.Error(), "app start failed") {
			t.Fatalf("restoreWithFallback() error = %v, want error containing 'app start failed'", err)
		}
		if gotFidelity != ateworkerpb.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES {
			t.Errorf("fidelity = %v, want %v", gotFidelity, ateworkerpb.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES)
		}
		// Final attempt's created containers remain in containersToDelete so
		// RestoreWorkload's deferred cleanup deletes them.
		if !slices.Equal(containersToDelete, []string{ocispec.SandboxID, "app"}) {
			t.Errorf("containersToDelete = %v, want [_sandbox app]", containersToDelete)
		}
		deleteContainers(context.Background(), rt, containersToDelete, "Restore")
		if gotTail := rt.calls[len(rt.calls)-2:]; !slices.Equal(gotTail, []string{"delete app", "delete _sandbox"}) {
			t.Errorf("final cleanup calls = %v, want [delete app delete _sandbox]", gotTail)
		}
	})
}
