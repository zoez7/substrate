#!/usr/bin/env bash

# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# This is sourced as part of install-ate.sh. Do not run directly.
#
# The counter demo: the worker pool is a CRD manifest, and the ActorTemplate
# is created through the ate API with `kubectl ate create actor-template`.
# The micro-VM variant additionally needs the cluster-wide `microvm`
# SandboxConfig from hack/install-microvm-deps.sh --install.

ATE_DEMOS+=(demo-counter) # register demo-counter
# The micro-VM variant is its own demo rather than a flag on demo-counter:
# that gets it into --help and into delete_all's teardown sweep for free, and
# the two can be installed side by side (the suites run against whichever the
# sandbox class under test selects).
ATE_DEMOS+=(demo-counter-microvm) # register demo-counter-microvm

demo-counter_usage() {
  echo "  --deploy-demo-counter-with-external-volume    Deploy demo-counter with external volume validation"
}

demo-counter_cmdline() {
  case "${1}" in
    --deploy-demo-counter) demo-counter_deploy "false" ;;
    --deploy-demo-counter-with-external-volume) demo-counter_deploy "true" ;;
    --delete-demo-counter) demo-counter_delete ;;
    *)
      return 1
      ;;
  esac
  return 0
}

demo-counter-microvm_cmdline() {
  case "${1}" in
    --deploy-demo-counter-microvm) demo-counter-microvm_deploy ;;
    --delete-demo-counter-microvm) demo-counter-microvm_delete ;;
    *)
      return 1
      ;;
  esac
  return 0
}

demo-counter_deploy() {
  local with_external_volume="${1:-false}"

  # The external-volume stanzas live in the template manifest as placeholders:
  # dropped for the plain deploy, substituted (in protojson shape) when the
  # validation path is on.
  local validate_cmd=("-e" "/\${VALIDATE_EXISTING_FILE_PATH_ARG}/d")
  local ext_vol_mount_cmd=("-e" "/\${EXTERNAL_VOLUME_MOUNTS}/d")
  local ext_vol_spec_cmd=("-e" "/\${EXTERNAL_VOLUMES}/d")
  if [[ "${with_external_volume}" == "true" ]]; then
    # csi-hostpath-sc only exists when hack/setup-csi-hostpath-kind.sh has run
    # (via SETUP_CSI=true). Otherwise fall back to the default "standard"
    # StorageClass.
    local storage_class="standard"
    if [[ "${SETUP_CSI:-false}" == "true" ]]; then
      storage_class="csi-hostpath-sc"
    fi

    validate_cmd=("-e" "s|\${VALIDATE_EXISTING_FILE_PATH_ARG}|  - --validate-existing-file-path=/external-data/test.txt|g")
    ext_vol_mount_cmd=("-e" "s|\${EXTERNAL_VOLUME_MOUNTS}|  - name: external-data\n    mountPath: /external-data|g")
    ext_vol_spec_cmd=("-e" "s|\${EXTERNAL_VOLUMES}|- name: external-data\n  type: ExternalVolumeTemplate\n  externalVolumeTemplate:\n    capacity: 1Gi\n    storageClassName: ${storage_class}|g")
  fi

  deploy_substrate_demo demo-counter \
    demos/counter/counter.yaml.tmpl \
    demos/counter/counter-template.yaml.tmpl \
    ate-demo-counter counter counter 300 \
    "${validate_cmd[@]}" "${ext_vol_mount_cmd[@]}" "${ext_vol_spec_cmd[@]}"
}

demo-counter_delete() {
  delete_substrate_demo demo-counter \
    demos/counter/counter.yaml.tmpl \
    ate-demo-counter counter
}

demo-counter-microvm_usage() {
  echo "  Needs hack/install-microvm-deps.sh --install to have run (cluster-wide microvm SandboxConfig)."
}

demo-counter-microvm_deploy() {
  # 600s golden budget: a micro-VM golden is a cloud-hypervisor cold boot
  # plus checkpoint, on nested KVM in CI.
  deploy_substrate_demo demo-counter-microvm \
    demos/counter/counter-microvm.yaml.tmpl \
    demos/counter/counter-microvm-template.yaml.tmpl \
    ate-demo-counter-microvm counter-microvm counter-microvm 600
}

demo-counter-microvm_delete() {
  delete_substrate_demo demo-counter-microvm \
    demos/counter/counter-microvm.yaml.tmpl \
    ate-demo-counter-microvm counter-microvm
}
