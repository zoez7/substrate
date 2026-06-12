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

package oidcprovider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/internal/localjwtauthority"
	"github.com/agent-substrate/substrate/internal/oidcjwt"
	"github.com/agent-substrate/substrate/internal/sessionidjwt"
)

// startProvider writes a fresh ES256 signing pool to disk and serves the
// provider endpoints for it, returning the server and the pool.
func startProvider(t *testing.T) (*httptest.Server, *localjwtauthority.Pool) {
	t.Helper()

	authority, err := localjwtauthority.GenerateECDSAP256Authority("key1")
	if err != nil {
		t.Fatalf("generate authority: %v", err)
	}
	pool := &localjwtauthority.Pool{Authorities: []*localjwtauthority.Authority{authority}}
	poolBytes, err := localjwtauthority.Marshal(pool)
	if err != nil {
		t.Fatalf("marshal pool: %v", err)
	}
	poolFile := filepath.Join(t.TempDir(), "pool.json")
	if err := os.WriteFile(poolFile, poolBytes, 0o600); err != nil {
		t.Fatalf("write pool file: %v", err)
	}

	// The issuer URL must be known before the handler serves, but the
	// handler builds jwks_uri from the configured issuer, so start the
	// server first and point a second provider at its URL.
	var provider *Provider
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provider.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	provider = New(srv.URL, poolFile)

	return srv, pool
}

func TestDiscoveryDocument(t *testing.T) {
	srv, _ := startProvider(t)

	resp, err := http.Get(srv.URL + "/.well-known/openid-configuration")
	if err != nil {
		t.Fatalf("fetch discovery doc: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var doc struct {
		Issuer      string   `json:"issuer"`
		JWKSURI     string   `json:"jwks_uri"`
		SigningAlgs []string `json:"id_token_signing_alg_values_supported"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("decode discovery doc: %v", err)
	}
	if doc.Issuer != srv.URL {
		t.Errorf("issuer = %q, want %q", doc.Issuer, srv.URL)
	}
	if doc.JWKSURI != srv.URL+"/openid/v1/jwks" {
		t.Errorf("jwks_uri = %q, want %q", doc.JWKSURI, srv.URL+"/openid/v1/jwks")
	}
	if len(doc.SigningAlgs) != 1 || doc.SigningAlgs[0] != "ES256" {
		t.Errorf("id_token_signing_alg_values_supported = %v, want [ES256]", doc.SigningAlgs)
	}
}

// TestMintedTokenRoundTrip proves the full loop: a token signed from the
// pool (as MintJWT signs it) verifies through the oidcjwt verifier using
// only the provider's published discovery document and JWKS.
func TestMintedTokenRoundTrip(t *testing.T) {
	srv, pool := startProvider(t)
	now := time.Now()

	claims := &sessionidjwt.Claims{
		Issuer:     srv.URL,
		Subject:    "apps/app1/users/user1/sessions/session1",
		Audiences:  []string{"ate-apiserver"},
		Expiration: now.Add(15 * time.Minute),
		NotBefore:  now.Add(-5 * time.Minute),
		IssuedAt:   now,
		JTI:        "test-jti",
		Substrate: sessionidjwt.SubstrateClaims{
			AppID:     "app1",
			UserID:    "user1",
			SessionID: "session1",
		},
	}
	wireClaims, err := sessionidjwt.ClaimsToWire(claims)
	if err != nil {
		t.Fatalf("claims to wire: %v", err)
	}
	authority := pool.Authorities[0]
	token, err := sessionidjwt.Sign(wireClaims, authority.SigningKey, authority.Algorithm, authority.ID)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	verifier := oidcjwt.NewVerifier(&oidcjwt.Config{Issuers: []*oidcjwt.IssuerConfig{{
		Issuer:    srv.URL,
		Kind:      "actor-jwt",
		IDClaim:   "sub",
		Audiences: []string{"ate-apiserver"},
	}}}, oidcjwt.Options{})

	got, err := verifier.Verify(context.Background(), token, now)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.ID != claims.Subject {
		t.Errorf("ID = %q, want %q", got.ID, claims.Subject)
	}
	substrate, err := got.Substrate()
	if err != nil {
		t.Fatalf("Substrate: %v", err)
	}
	if substrate.AppID != "app1" || substrate.UserID != "user1" || substrate.SessionID != "session1" {
		t.Errorf("Substrate = %+v, want app1/user1/session1", substrate)
	}
}

func TestMissingPoolFile(t *testing.T) {
	provider := New("https://broker.example", filepath.Join(t.TempDir(), "does-not-exist.json"))
	srv := httptest.NewServer(provider.Handler())
	t.Cleanup(srv.Close)

	for _, path := range []string{"/.well-known/openid-configuration", "/openid/v1/jwks"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("fetch %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusInternalServerError {
			t.Errorf("GET %s status = %d, want 500", path, resp.StatusCode)
		}
	}
}
