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

package oidcjwt

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/agent-substrate/substrate/internal/sessionidjwt"
)

// KubernetesClaims covers the Kubernetes-specific claims carried by a bound
// service account JWT. The standard RFC7519 claims live on VerifiedToken.
//
// Verification does not check the object binding claims. If needed for your
// use case, you will need to check the object bindings by connecting to the
// cluster and seeing if the object(s) the bindings name still exist within
// the cluster.
type KubernetesClaims struct {
	Namespace string

	ServiceAccountName string
	ServiceAccountUID  string
	PodName            string
	PodUID             string
	SecretName         string
	SecretUID          string
	NodeName           string
	NodeUID            string

	WarnAfter time.Time
}

type parseBoundClaims struct {
	Namespace      string                    `json:"namespace,omitempty"`
	Pod            parseBoundObjectReference `json:"pod,omitempty"`
	ServiceAccount parseBoundObjectReference `json:"serviceaccount,omitempty"`
	Secret         parseBoundObjectReference `json:"secret,omitempty"`
	Node           parseBoundObjectReference `json:"node,omitempty"`
	WarnAfter      float64                   `json:"warnafter,omitempty"`
}

type parseBoundObjectReference struct {
	Name string `json:"name,omitempty"`
	UID  string `json:"uid,omitempty"`
}

// Kubernetes extracts the Kubernetes bound service account claims from the
// verified payload. Fields absent from the token are left zero.
func (t *VerifiedToken) Kubernetes() (*KubernetesClaims, error) {
	var wire struct {
		BoundClaims parseBoundClaims `json:"kubernetes.io,omitempty"`
	}
	if err := json.Unmarshal(t.payload, &wire); err != nil {
		return nil, fmt.Errorf("while unmarshaling kubernetes claims: %w", err)
	}
	return &KubernetesClaims{
		Namespace:          wire.BoundClaims.Namespace,
		ServiceAccountName: wire.BoundClaims.ServiceAccount.Name,
		ServiceAccountUID:  wire.BoundClaims.ServiceAccount.UID,
		PodName:            wire.BoundClaims.Pod.Name,
		PodUID:             wire.BoundClaims.Pod.UID,
		SecretName:         wire.BoundClaims.Secret.Name,
		SecretUID:          wire.BoundClaims.Secret.UID,
		NodeName:           wire.BoundClaims.Node.Name,
		NodeUID:            wire.BoundClaims.Node.UID,
		WarnAfter:          time.Unix(int64(wire.BoundClaims.WarnAfter), 0),
	}, nil
}

// Substrate extracts the substrate session claims (the "ate.dev" claim minted
// by the session-id broker) from the verified payload. Fields absent from the
// token are left zero.
func (t *VerifiedToken) Substrate() (*sessionidjwt.SubstrateClaims, error) {
	var wire struct {
		Substrate sessionidjwt.WireSubstrateClaims `json:"ate.dev,omitempty"`
	}
	if err := json.Unmarshal(t.payload, &wire); err != nil {
		return nil, fmt.Errorf("while unmarshaling substrate claims: %w", err)
	}
	return &sessionidjwt.SubstrateClaims{
		AppID:     wire.Substrate.AppID,
		UserID:    wire.Substrate.UserID,
		SessionID: wire.Substrate.SessionID,
	}, nil
}
