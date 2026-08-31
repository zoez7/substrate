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

// Package counter installs the counter demo: a counter actor exercising
// snapshot, resume, and atenet ingress, optionally with an external volume
// attached so the CSI path is covered end to end.
package counter

import (
	"github.com/spf13/pflag"

	"github.com/agent-substrate/substrate/cmd/ate-setup/internal/demos"
	"github.com/agent-substrate/substrate/cmd/ate-setup/internal/steps"
)

const (
	atespace         = "ate-demo-counter"
	poolTemplate     = "demos/counter/counter.yaml.tmpl"
	templateManifest = "demos/counter/counter-template.yaml.tmpl"
)

// externalVolumeValues fills the template manifest's external-volume
// placeholders, in protojson shape (the volume union carries an explicit
// type discriminator).
func externalVolumeValues(e *steps.Env) map[string]string {
	storageClass := "standard"
	if e.Cfg.Kind {
		storageClass = "csi-hostpath-sc"
	}
	return map[string]string{
		"VALIDATE_EXISTING_FILE_PATH_ARG": "  - --validate-existing-file-path=/external-data/test.txt",
		"EXTERNAL_VOLUME_MOUNTS": "  - name: external-data\n" +
			"    mountPath: /external-data",
		"EXTERNAL_VOLUMES": "- name: external-data\n" +
			"  type: ExternalVolumeTemplate\n" +
			"  externalVolumeTemplate:\n" +
			"    capacity: 1Gi\n" +
			"    storageClassName: " + storageClass,
	}
}

// demo is the counter demo, which can optionally be deployed with an external
// volume attached.
type demo struct {
	demos.Simple

	withExternalVolume bool
}

func init() {
	d := &demo{Simple: demos.Simple{
		DemoName:    "demo-counter",
		Short:       "A counter actor exercising snapshot, resume, and atenet ingress",
		Template:    poolTemplate,
		Deployments: []steps.TemplateRef{{Atespace: atespace, Name: "counter"}},
		TemplateManifests: []steps.TemplateManifest{{
			Path: templateManifest,
			Ref:  steps.TemplateRef{Atespace: atespace, Name: "counter"},
		}},
	}}
	d.TemplateExtraValues = func(e *steps.Env) map[string]string {
		if !d.withExternalVolume {
			return nil
		}
		return externalVolumeValues(e)
	}
	demos.Register(d)
}

func (d *demo) Flags(fs *pflag.FlagSet) {
	fs.BoolVar(&d.withExternalVolume, "with-external-volume", false,
		"Attach an external volume and validate a pre-seeded file on it (run \"setup csi\" first)")
}
