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
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// signToken builds and signs a JWT over the given claims. Only the
// algorithms the tests need are implemented.
func signToken(t *testing.T, key crypto.Signer, alg, kid string, claims map[string]any) string {
	t.Helper()

	headerBytes, err := json.Marshal(map[string]any{"alg": alg, "kid": kid})
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	payloadBytes, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	toBeSigned := base64.RawURLEncoding.EncodeToString(headerBytes) + "." + base64.RawURLEncoding.EncodeToString(payloadBytes)
	digest := sha256.Sum256([]byte(toBeSigned))

	var sigBytes []byte
	switch alg {
	case "ES256":
		ecKey := key.(*ecdsa.PrivateKey)
		r, s, err := ecdsa.Sign(rand.Reader, ecKey, digest[:])
		if err != nil {
			t.Fatalf("ecdsa sign: %v", err)
		}
		sigBytes = make([]byte, 2*32)
		r.FillBytes(sigBytes[:32])
		s.FillBytes(sigBytes[32:])
	case "RS256":
		rsaKey := key.(*rsa.PrivateKey)
		sigBytes, err = rsa.SignPKCS1v15(rand.Reader, rsaKey, crypto.SHA256, digest[:])
		if err != nil {
			t.Fatalf("rsa sign: %v", err)
		}
	default:
		t.Fatalf("unsupported test algorithm %q", alg)
	}

	return toBeSigned + "." + base64.RawURLEncoding.EncodeToString(sigBytes)
}

func ecJWK(t *testing.T, kid string, pub *ecdsa.PublicKey) map[string]any {
	t.Helper()

	// The uncompressed point: 0x04 || X || Y, 32 bytes per coordinate.
	raw, err := pub.Bytes()
	if err != nil {
		t.Fatalf("encode EC public key: %v", err)
	}
	return map[string]any{
		"kty": "EC",
		"kid": kid,
		"crv": "P-256",
		"x":   base64.RawURLEncoding.EncodeToString(raw[1:33]),
		"y":   base64.RawURLEncoding.EncodeToString(raw[33:]),
	}
}

func rsaJWK(kid string, pub *rsa.PublicKey) map[string]any {
	e := rsaExponentBytes(pub.E)
	return map[string]any{
		"kty": "RSA",
		"kid": kid,
		"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(e),
	}
}

// rsaExponentBytes encodes the RSA public exponent big-endian without
// leading zeros, as JWKs carry it.
func rsaExponentBytes(e int) []byte {
	b := []byte{byte(e >> 16), byte(e >> 8), byte(e)}
	for len(b) > 1 && b[0] == 0 {
		b = b[1:]
	}
	return b
}

// startIssuerServer serves an OIDC discovery document and a JWKS containing
// the given keys, mimicking a real issuer.
func startIssuerServer(t *testing.T, jwks ...map[string]any) *httptest.Server {
	t.Helper()

	jwksBytes, err := json.Marshal(map[string]any{"keys": jwks})
	if err != nil {
		t.Fatalf("marshal jwks: %v", err)
	}

	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"jwks_uri":%q}`, srv.URL+"/jwks")
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		w.Write(jwksBytes)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// baseClaims returns standard claims valid at now for the given issuer.
func baseClaims(issuer, subject string, audiences []string, now time.Time) map[string]any {
	return map[string]any{
		"iss": issuer,
		"sub": subject,
		"aud": audiences,
		"exp": float64(now.Add(15 * time.Minute).Unix()),
		"nbf": float64(now.Add(-5 * time.Minute).Unix()),
		"iat": float64(now.Unix()),
		"jti": "test-jti",
	}
}

func TestVerify(t *testing.T) {
	now := time.Now()

	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate EC key: %v", err)
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}

	actorSrv := startIssuerServer(t, ecJWK(t, "ec1", &ecKey.PublicKey))
	clientSrv := startIssuerServer(t, rsaJWK("rsa1", &rsaKey.PublicKey))

	cfg := &Config{Issuers: []*IssuerConfig{
		{Issuer: actorSrv.URL, Kind: "actor-jwt", IDClaim: "sub", Audiences: []string{"ate-apiserver"}},
		{Issuer: clientSrv.URL, Kind: "k8s-jwt", IDClaim: "email", Audiences: []string{"ate-apiserver"}},
	}}
	v := NewVerifier(cfg, Options{})

	actorSubject := "apps/app1/users/user1/sessions/session1"
	actorClaims := baseClaims(actorSrv.URL, actorSubject, []string{"ate-apiserver"}, now)
	actorClaims["ate.dev"] = map[string]any{"appID": "app1", "userID": "user1", "sessionID": "session1"}
	actorToken := signToken(t, ecKey, "ES256", "ec1", actorClaims)

	clientClaims := baseClaims(clientSrv.URL, "system:serviceaccount:ns1:sa1", []string{"ate-apiserver"}, now)
	clientClaims["email"] = "robot@example.com"
	clientClaims["kubernetes.io"] = map[string]any{
		"namespace":      "ns1",
		"serviceaccount": map[string]any{"name": "sa1", "uid": "sa-uid"},
		"pod":            map[string]any{"name": "pod1", "uid": "pod-uid"},
	}
	clientToken := signToken(t, rsaKey, "RS256", "rsa1", clientClaims)

	// Tamper with the payload of a validly-signed token.
	tamperedClaims := baseClaims(actorSrv.URL, "apps/evil/users/evil/sessions/evil", []string{"ate-apiserver"}, now)
	tamperedPayload, err := json.Marshal(tamperedClaims)
	if err != nil {
		t.Fatalf("marshal tampered claims: %v", err)
	}
	actorSegments := strings.Split(actorToken, ".")
	tamperedToken := actorSegments[0] + "." + base64.RawURLEncoding.EncodeToString(tamperedPayload) + "." + actorSegments[2]

	expiredClaims := baseClaims(actorSrv.URL, actorSubject, []string{"ate-apiserver"}, now.Add(-2*time.Hour))
	expiredToken := signToken(t, ecKey, "ES256", "ec1", expiredClaims)

	wrongAudClaims := baseClaims(actorSrv.URL, actorSubject, []string{"someone-else"}, now)
	wrongAudToken := signToken(t, ecKey, "ES256", "ec1", wrongAudClaims)

	unknownKidToken := signToken(t, ecKey, "ES256", "nope", actorClaims)

	unknownIssuerClaims := baseClaims("http://unknown.invalid", actorSubject, []string{"ate-apiserver"}, now)
	unknownIssuerToken := signToken(t, ecKey, "ES256", "ec1", unknownIssuerClaims)

	noIDClaims := baseClaims(clientSrv.URL, "system:serviceaccount:ns1:sa1", []string{"ate-apiserver"}, now)
	noIDToken := signToken(t, rsaKey, "RS256", "rsa1", noIDClaims)

	tests := []struct {
		name     string
		token    string
		wantErr  bool
		wantKind string
		wantID   string
	}{
		{
			name:     "valid ES256 actor token",
			token:    actorToken,
			wantKind: "actor-jwt",
			wantID:   actorSubject,
		},
		{
			name:     "valid RS256 client token with custom ID claim",
			token:    clientToken,
			wantKind: "k8s-jwt",
			wantID:   "robot@example.com",
		},
		{
			name:    "tampered payload",
			token:   tamperedToken,
			wantErr: true,
		},
		{
			name:    "expired token",
			token:   expiredToken,
			wantErr: true,
		},
		{
			name:    "unaccepted audience",
			token:   wrongAudToken,
			wantErr: true,
		},
		{
			name:    "unknown key ID",
			token:   unknownKidToken,
			wantErr: true,
		},
		{
			name:    "unconfigured issuer",
			token:   unknownIssuerToken,
			wantErr: true,
		},
		{
			name:    "missing ID claim",
			token:   noIDToken,
			wantErr: true,
		},
		{
			name:    "malformed token",
			token:   "not-a-jwt",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := v.Verify(context.Background(), tt.token, now)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Verify succeeded, want error")
				}
				if errors.Is(err, ErrUnavailable) {
					t.Errorf("Verify error = %v, should not be ErrUnavailable", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if got.Issuer.Kind != tt.wantKind {
				t.Errorf("Kind = %q, want %q", got.Issuer.Kind, tt.wantKind)
			}
			if got.ID != tt.wantID {
				t.Errorf("ID = %q, want %q", got.ID, tt.wantID)
			}
		})
	}

	t.Run("substrate claims", func(t *testing.T) {
		got, err := v.Verify(context.Background(), actorToken, now)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		sub, err := got.Substrate()
		if err != nil {
			t.Fatalf("Substrate: %v", err)
		}
		if sub.AppID != "app1" || sub.UserID != "user1" || sub.SessionID != "session1" {
			t.Errorf("Substrate = %+v, want app1/user1/session1", sub)
		}
	})

	t.Run("kubernetes claims", func(t *testing.T) {
		got, err := v.Verify(context.Background(), clientToken, now)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		k8s, err := got.Kubernetes()
		if err != nil {
			t.Fatalf("Kubernetes: %v", err)
		}
		if k8s.Namespace != "ns1" {
			t.Errorf("Namespace = %q, want %q", k8s.Namespace, "ns1")
		}
		if k8s.ServiceAccountName != "sa1" {
			t.Errorf("ServiceAccountName = %q, want %q", k8s.ServiceAccountName, "sa1")
		}
		if k8s.PodName != "pod1" {
			t.Errorf("PodName = %q, want %q", k8s.PodName, "pod1")
		}
	})
}

func TestVerifyUnavailable(t *testing.T) {
	now := time.Now()

	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate EC key: %v", err)
	}

	// An issuer whose discovery endpoint always fails.
	brokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(brokenSrv.Close)

	// An issuer that is no longer reachable at all.
	closedSrv := httptest.NewServer(http.NewServeMux())
	closedURL := closedSrv.URL
	closedSrv.Close()

	cfg := &Config{Issuers: []*IssuerConfig{
		{Issuer: brokenSrv.URL, Kind: "actor-jwt", IDClaim: "sub", Audiences: []string{"aud"}},
		{Issuer: closedURL, Kind: "actor-jwt", IDClaim: "sub", Audiences: []string{"aud"}},
	}}
	v := NewVerifier(cfg, Options{})

	for _, issuer := range []string{brokenSrv.URL, closedURL} {
		token := signToken(t, ecKey, "ES256", "ec1", baseClaims(issuer, "subject", []string{"aud"}, now))
		_, err := v.Verify(context.Background(), token, now)
		if !errors.Is(err, ErrUnavailable) {
			t.Errorf("Verify(%s) error = %v, want ErrUnavailable", issuer, err)
		}
	}
}

func TestVerifyCustomRootCAs(t *testing.T) {
	now := time.Now()

	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate EC key: %v", err)
	}

	jwksBytes, err := json.Marshal(map[string]any{"keys": []map[string]any{ecJWK(t, "ec1", &ecKey.PublicKey)}})
	if err != nil {
		t.Fatalf("marshal jwks: %v", err)
	}
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"jwks_uri":%q}`, srv.URL+"/jwks")
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		w.Write(jwksBytes)
	})
	srv = httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)

	cfg := &Config{Issuers: []*IssuerConfig{
		{Issuer: srv.URL, Kind: "actor-jwt", IDClaim: "sub", Audiences: []string{"aud"}},
	}}
	token := signToken(t, ecKey, "ES256", "ec1", baseClaims(srv.URL, "subject", []string{"aud"}, now))

	// Without the server's CA, the fetch fails as unavailable.
	v := NewVerifier(cfg, Options{})
	if _, err := v.Verify(context.Background(), token, now); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Verify without RootCAs error = %v, want ErrUnavailable", err)
	}

	// With the server's CA in RootCAs, verification succeeds.
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	v = NewVerifier(cfg, Options{RootCAs: pool})
	got, err := v.Verify(context.Background(), token, now)
	if err != nil {
		t.Fatalf("Verify with RootCAs: %v", err)
	}
	if got.ID != "subject" {
		t.Errorf("ID = %q, want %q", got.ID, "subject")
	}
}

func TestUnmarshal(t *testing.T) {
	tests := []struct {
		name    string
		wire    string
		wantErr bool
		check   func(t *testing.T, cfg *Config)
	}{
		{
			name: "valid with default ID claim",
			wire: `{"issuers":[{"issuer":"https://a","kind":"actor-jwt","audiences":["x"]}]}`,
			check: func(t *testing.T, cfg *Config) {
				if len(cfg.Issuers) != 1 {
					t.Fatalf("len(Issuers) = %d, want 1", len(cfg.Issuers))
				}
				if cfg.Issuers[0].IDClaim != "sub" {
					t.Errorf("IDClaim = %q, want %q", cfg.Issuers[0].IDClaim, "sub")
				}
			},
		},
		{
			name: "valid with explicit ID claim",
			wire: `{"issuers":[{"issuer":"https://a","kind":"k8s-jwt","idClaim":"email","audiences":["x"]}]}`,
			check: func(t *testing.T, cfg *Config) {
				if cfg.Issuers[0].IDClaim != "email" {
					t.Errorf("IDClaim = %q, want %q", cfg.Issuers[0].IDClaim, "email")
				}
			},
		},
		{
			name:    "missing issuer URL",
			wire:    `{"issuers":[{"kind":"actor-jwt","audiences":["x"]}]}`,
			wantErr: true,
		},
		{
			name:    "missing kind",
			wire:    `{"issuers":[{"issuer":"https://a","audiences":["x"]}]}`,
			wantErr: true,
		},
		{
			name:    "empty audiences",
			wire:    `{"issuers":[{"issuer":"https://a","kind":"actor-jwt","audiences":[]}]}`,
			wantErr: true,
		},
		{
			name:    "duplicate issuer",
			wire:    `{"issuers":[{"issuer":"https://a","kind":"actor-jwt","audiences":["x"]},{"issuer":"https://a","kind":"k8s-jwt","audiences":["x"]}]}`,
			wantErr: true,
		},
		{
			name:    "null entry",
			wire:    `{"issuers":[null]}`,
			wantErr: true,
		},
		{
			name:    "invalid JSON",
			wire:    `{`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Unmarshal([]byte(tt.wire))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Unmarshal succeeded, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if tt.check != nil {
				tt.check(t, cfg)
			}
		})
	}
}
