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

// Package oidcprovider serves the OIDC discovery document and JWKS endpoint
// that make the session-id broker a real OIDC issuer. Relying parties (the
// ate-apiserver's own authentication interceptor among them) discover the
// broker's verification keys from issuer + /.well-known/openid-configuration
// instead of needing the signing pool file.
package oidcprovider

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/agent-substrate/substrate/internal/localjwtauthority"
)

// jwksPath is where the JWK Set is served, relative to the issuer URL.
const jwksPath = "/openid/v1/jwks"

// Provider serves the OIDC issuer endpoints for the session-id JWT pool.
type Provider struct {
	issuer   string
	poolFile string
}

// New returns a Provider that advertises issuer and publishes the public
// keys of the signing pool stored at poolFile.
func New(issuer, poolFile string) *Provider {
	return &Provider{issuer: issuer, poolFile: poolFile}
}

// Handler returns the HTTP handler serving the discovery document and JWKS.
func (p *Provider) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", p.serveDiscovery)
	mux.HandleFunc("GET "+jwksPath, p.serveJWKS)
	return mux
}

// discoveryDoc is the subset of the OIDC discovery metadata relying parties
// need to locate and use the JWKS.
type discoveryDoc struct {
	Issuer        string   `json:"issuer"`
	JWKSURI       string   `json:"jwks_uri"`
	ResponseTypes []string `json:"response_types_supported"`
	SubjectTypes  []string `json:"subject_types_supported"`
	SigningAlgs   []string `json:"id_token_signing_alg_values_supported"`
}

func (p *Provider) serveDiscovery(w http.ResponseWriter, r *http.Request) {
	pool, err := p.loadPool()
	if err != nil {
		slog.ErrorContext(r.Context(), "Failed to load signing pool for discovery document", slog.Any("err", err))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	algSet := make(map[string]struct{})
	var algs []string
	for _, authority := range pool.Authorities {
		if _, ok := algSet[authority.Algorithm]; ok {
			continue
		}
		algSet[authority.Algorithm] = struct{}{}
		algs = append(algs, authority.Algorithm)
	}

	doc := discoveryDoc{
		Issuer:        p.issuer,
		JWKSURI:       p.issuer + jwksPath,
		ResponseTypes: []string{"id_token"},
		SubjectTypes:  []string{"public"},
		SigningAlgs:   algs,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(doc); err != nil {
		slog.ErrorContext(r.Context(), "Failed to write discovery document", slog.Any("err", err))
	}
}

func (p *Provider) serveJWKS(w http.ResponseWriter, r *http.Request) {
	pool, err := p.loadPool()
	if err != nil {
		slog.ErrorContext(r.Context(), "Failed to load signing pool for JWKS", slog.Any("err", err))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	jwksBytes, err := localjwtauthority.MarshalJWKS(pool)
	if err != nil {
		slog.ErrorContext(r.Context(), "Failed to marshal JWKS", slog.Any("err", err))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/jwk-set+json")
	if _, err := w.Write(jwksBytes); err != nil {
		slog.ErrorContext(r.Context(), "Failed to write JWKS", slog.Any("err", err))
	}
}

// loadPool reads the signing pool from disk. Like the signing path in
// sessionidentity, it re-reads per request so key rotations are picked up;
// caching is a named follow-up.
func (p *Provider) loadPool() (*localjwtauthority.Pool, error) {
	poolBytes, err := os.ReadFile(p.poolFile)
	if err != nil {
		return nil, fmt.Errorf("while reading signing pool: %w", err)
	}
	pool, err := localjwtauthority.Unmarshal(poolBytes)
	if err != nil {
		return nil, fmt.Errorf("while unmarshaling signing pool: %w", err)
	}
	return pool, nil
}
