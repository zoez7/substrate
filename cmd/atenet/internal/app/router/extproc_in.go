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

package router

import (
	"fmt"
	"net"
	"strings"

	"github.com/agent-substrate/substrate/internal/atemetadata"
	"github.com/agent-substrate/substrate/internal/resources"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
)

type requestMetadata struct {
	headers map[string]string
	path    string
	host    string
}

// redactedHeaders are credential-bearing headers whose values must never be
// logged.
var redactedHeaders = map[string]struct{}{
	"authorization":             {},
	atemetadata.ForwardedJWTKey: {},
}

func (m *requestMetadata) String() string {
	headers := make(map[string]string, len(m.headers))
	for k, v := range m.headers {
		if _, ok := redactedHeaders[k]; ok {
			v = "[REDACTED]"
		}
		headers[k] = v
	}
	return fmt.Sprintf("{headers:%v path:%s host:%s}", headers, m.path, m.host)
}

// bearerToken returns the Bearer token from the request's authorization
// header, or "" when the header is absent or not a Bearer credential.
func (m *requestMetadata) bearerToken() string {
	token, ok := strings.CutPrefix(m.headers["authorization"], "Bearer ")
	if !ok {
		return ""
	}
	return token
}

func newRequestMetadata(headers []*corev3.HeaderValue) *requestMetadata {
	headersMap := make(map[string]string)
	var path string
	var host string

	for _, h := range headers {
		// Note: Lowercases all headers.
		k := strings.ToLower(h.Key)
		val := h.Value
		if val == "" && len(h.RawValue) > 0 {
			val = string(h.RawValue)
		}

		headersMap[k] = val
		if k == ":path" {
			path = val
		}
		if k == ":authority" || k == "host" {
			host = val
		}
	}

	return &requestMetadata{
		headers: headersMap,
		path:    path,
		host:    host,
	}
}

func parseActorID(host string) (string, error) {
	var err error
	if strings.Contains(host, ":") {
		host, _, err = net.SplitHostPort(host)
	}
	if err != nil {
		return "", err
	}
	actorID, found := strings.CutSuffix(strings.TrimSuffix(host, "."), "."+resources.ActorDNSSuffix)
	if !found {
		return "", fmt.Errorf("invalid actor_id: must end with %s, got %q", resources.ActorDNSSuffix, host)
	}
	if err := resources.ValidateActorID(actorID); err != nil {
		return "", err
	}

	return actorID, nil
}
