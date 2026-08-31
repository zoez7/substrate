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

ATE_DEMOS+=(demo-multi-template) # register demo-multi-template

demo-multi-template_cmdline() {
  case "${1}" in
    --deploy-demo-multi-template) demo-multi-template_deploy ;;
    --delete-demo-multi-template) demo-multi-template_delete ;;
    *)
      return 1
      ;;
  esac
  return 0
}

demo-multi-template_deploy() {
  log_step "demo-multi-template_deploy"
  ensure_crds
  sed "s|\${BUCKET_NAME}|${BUCKET_NAME}|g" demos/multi-template/multi-template.yaml.tmpl \
    | run_ko apply -f -

  log_step "Waiting for the shared-pool worker pool rollout..."
  wait_for_pool_rollout_fatal shared-pool ate-demo-multi-template-pool

  # Two templates in two atespaces, one pool: the composed form of what
  # deploy_substrate_demo does for single-template demos.
  create_substrate_template \
    demos/multi-template/multi-template-counter-template.yaml.tmpl \
    ate-demo-multi-template-counter counter
  create_substrate_template \
    demos/multi-template/multi-template-fspersist-template.yaml.tmpl \
    ate-demo-multi-template-fspersist fspersist

  # Wait for both templates' golden snapshots before returning.
  log_step "Waiting for the multi-template golden snapshots..."
  if ! wait_actortemplate_ready ate-demo-multi-template-counter counter 300; then
    exit 1
  fi
  if ! wait_actortemplate_ready ate-demo-multi-template-fspersist fspersist 300; then
    exit 1
  fi
}

demo-multi-template_delete() {
  log_step "demo-multi-template_delete"
  delete_substrate_templates ate-demo-multi-template-counter counter
  delete_substrate_templates ate-demo-multi-template-fspersist fspersist
  sed "s|\${BUCKET_NAME}|${BUCKET_NAME}|g" demos/multi-template/multi-template.yaml.tmpl \
    | run_kubectl delete --ignore-not-found -f -
}
