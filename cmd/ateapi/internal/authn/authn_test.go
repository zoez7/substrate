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

package authn

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/internal/atemetadata"
	"github.com/agent-substrate/substrate/internal/localca"
	"github.com/agent-substrate/substrate/internal/localjwtauthority"
	"github.com/agent-substrate/substrate/internal/oidcjwt"
	"github.com/agent-substrate/substrate/internal/sessionidjwt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

const (
	routerName = "atenet-router.ate-system.svc"
	apiName    = "api.ate-system.svc"
	audience   = "ate-apiserver"
)

// signLeaf creates a leaf certificate from template signed by the given CA.
func signLeaf(t *testing.T, ca *localca.CA, template *x509.Certificate) *x509.Certificate {
	t.Helper()

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}

	template.SerialNumber = big.NewInt(1)
	template.NotBefore = time.Now().Add(-time.Minute)
	template.NotAfter = time.Now().Add(time.Hour)
	template.KeyUsage = x509.KeyUsageDigitalSignature
	template.BasicConstraintsValid = true

	der, err := x509.CreateCertificate(rand.Reader, template, ca.RootCertificate, pub, ca.SigningKey)
	if err != nil {
		t.Fatalf("create leaf cert: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse leaf cert: %v", err)
	}
	return leaf
}

func rootPool(ca *localca.CA) *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(ca.RootCertificate)
	return pool
}

// ctxWithCert returns a context carrying leaf as the peer's certificate, as a
// gRPC mTLS handshake would. A nil leaf produces a peer with no certificate.
// Metadata pairs, if any, are attached as incoming metadata.
func ctxWithCert(leaf *x509.Certificate, mdPairs ...string) context.Context {
	state := tls.ConnectionState{}
	if leaf != nil {
		state.PeerCertificates = []*x509.Certificate{leaf}
	}
	ctx := peer.NewContext(context.Background(), &peer.Peer{
		AuthInfo: credentials.TLSInfo{State: state},
	})
	if len(mdPairs) > 0 {
		ctx = metadata.NewIncomingContext(ctx, metadata.Pairs(mdPairs...))
	}
	return ctx
}

// startIssuer generates a fresh ES256 signing authority and serves OIDC
// discovery and JWKS for it, mimicking the session-id broker.
func startIssuer(t *testing.T) (*httptest.Server, *localjwtauthority.Authority) {
	t.Helper()

	authority, err := localjwtauthority.GenerateECDSAP256Authority("key1")
	if err != nil {
		t.Fatalf("generate authority: %v", err)
	}
	jwksBytes, err := localjwtauthority.MarshalJWKS(&localjwtauthority.Pool{Authorities: []*localjwtauthority.Authority{authority}})
	if err != nil {
		t.Fatalf("marshal JWKS: %v", err)
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
	return srv, authority
}

// mintToken signs a session JWT for the given issuer, as MintJWT would.
func mintToken(t *testing.T, issuer string, authority *localjwtauthority.Authority, subject string, expiration time.Time) string {
	t.Helper()

	now := time.Now()
	wireClaims, err := sessionidjwt.ClaimsToWire(&sessionidjwt.Claims{
		Issuer:     issuer,
		Subject:    subject,
		Audiences:  []string{audience},
		Expiration: expiration,
		NotBefore:  now.Add(-5 * time.Minute),
		IssuedAt:   now,
		JTI:        "test-jti",
		Substrate: sessionidjwt.SubstrateClaims{
			AppID:     "app1",
			UserID:    "user1",
			SessionID: "session1",
		},
	})
	if err != nil {
		t.Fatalf("claims to wire: %v", err)
	}
	token, err := sessionidjwt.Sign(wireClaims, authority.SigningKey, authority.Algorithm, authority.ID)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return token
}

func TestAuthenticate(t *testing.T) {
	systemCA, err := localca.GenerateED25519CA("system-ca")
	if err != nil {
		t.Fatalf("generate system CA: %v", err)
	}
	otherCA, err := localca.GenerateED25519CA("other-ca")
	if err != nil {
		t.Fatalf("generate other CA: %v", err)
	}

	routerLeaf := signLeaf(t, systemCA, &x509.Certificate{
		DNSNames:    []string{routerName},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	apiLeaf := signLeaf(t, systemCA, &x509.Certificate{
		DNSNames:    []string{apiName},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	})
	// A system cert whose DNS SAN is not allowlisted is not classified.
	unlistedLeaf := signLeaf(t, systemCA, &x509.Certificate{
		DNSNames:    []string{"evil.example.com"},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	// A system cert carrying only a CommonName and no SAN is not classified:
	// there is no CommonName fallback.
	cnOnlyLeaf := signLeaf(t, systemCA, &x509.Certificate{
		Subject:     pkix.Name{CommonName: apiName},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	// A leaf signed by an unrelated CA (e.g. a workerpool cert) is accepted at
	// the TLS layer but not classified.
	otherLeaf := signLeaf(t, otherCA, &x509.Certificate{
		Subject:     pkix.Name{CommonName: "stranger"},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})

	actorSrv, actorAuthority := startIssuer(t)
	clientSrv, clientAuthority := startIssuer(t)

	// An issuer that is configured but unreachable.
	downSrv := httptest.NewServer(http.NewServeMux())
	downURL := downSrv.URL
	downSrv.Close()

	verifier := oidcjwt.NewVerifier(&oidcjwt.Config{Issuers: []*oidcjwt.IssuerConfig{
		{Issuer: actorSrv.URL, Kind: "actor-jwt", IDClaim: "sub", Audiences: []string{audience}},
		{Issuer: clientSrv.URL, Kind: "k8s-jwt", IDClaim: "sub", Audiences: []string{audience}},
		{Issuer: downURL, Kind: "k8s-jwt", IDClaim: "sub", Audiences: []string{audience}},
	}}, oidcjwt.Options{})

	actorSubject := "apps/app1/users/user1/sessions/session1"
	actorToken := mintToken(t, actorSrv.URL, actorAuthority, actorSubject, time.Now().Add(15*time.Minute))
	expiredToken := mintToken(t, actorSrv.URL, actorAuthority, actorSubject, time.Now().Add(-time.Hour))
	clientToken := mintToken(t, clientSrv.URL, clientAuthority, "system:serviceaccount:ns1:sa1", time.Now().Add(15*time.Minute))
	downToken := mintToken(t, downURL, clientAuthority, "anyone", time.Now().Add(15*time.Minute))

	a := New(Config{
		SystemRoots:       rootPool(systemCA),
		SystemNames:       []string{apiName, routerName},
		TrustedForwarders: []string{routerName},
		Verifier:          verifier,
	})

	tests := []struct {
		name          string
		ctx           context.Context
		wantCode      codes.Code
		wantKind      Kind
		wantID        string
		wantForwarder string
	}{
		{
			name:     "system cert",
			ctx:      ctxWithCert(apiLeaf),
			wantKind: KindSystem,
			wantID:   apiName,
		},
		{
			name:          "forwarded jwt from trusted forwarder",
			ctx:           ctxWithCert(routerLeaf, atemetadata.ForwardedJWTKey, actorToken),
			wantKind:      KindActorJWT,
			wantID:        actorSubject,
			wantForwarder: routerName,
		},
		{
			name:     "forwarded jwt from non-forwarder system peer",
			ctx:      ctxWithCert(apiLeaf, atemetadata.ForwardedJWTKey, actorToken),
			wantCode: codes.Unauthenticated,
		},
		{
			name:     "forwarded jwt without client cert",
			ctx:      ctxWithCert(nil, atemetadata.ForwardedJWTKey, actorToken),
			wantCode: codes.Unauthenticated,
		},
		{
			name:     "forwarded jwt from unclassified cert",
			ctx:      ctxWithCert(otherLeaf, atemetadata.ForwardedJWTKey, actorToken),
			wantCode: codes.Unauthenticated,
		},
		{
			name:     "invalid forwarded jwt from trusted forwarder",
			ctx:      ctxWithCert(routerLeaf, atemetadata.ForwardedJWTKey, "garbage"),
			wantCode: codes.Unauthenticated,
		},
		{
			name:     "expired forwarded jwt from trusted forwarder",
			ctx:      ctxWithCert(routerLeaf, atemetadata.ForwardedJWTKey, expiredToken),
			wantCode: codes.Unauthenticated,
		},
		{
			name:     "system cert ignores bearer token",
			ctx:      ctxWithCert(apiLeaf, "authorization", "Bearer garbage"),
			wantKind: KindSystem,
			wantID:   apiName,
		},
		{
			name:     "self-asserted bearer token",
			ctx:      ctxWithCert(nil, "authorization", "Bearer "+clientToken),
			wantKind: KindK8sJWT,
			wantID:   "system:serviceaccount:ns1:sa1",
		},
		{
			name:     "self-asserted bearer token over unclassified cert",
			ctx:      ctxWithCert(otherLeaf, "authorization", "Bearer "+clientToken),
			wantKind: KindK8sJWT,
			wantID:   "system:serviceaccount:ns1:sa1",
		},
		{
			name:     "invalid bearer token",
			ctx:      ctxWithCert(nil, "authorization", "Bearer garbage"),
			wantCode: codes.Unauthenticated,
		},
		{
			name:     "non-Bearer authorization header",
			ctx:      ctxWithCert(nil, "authorization", "Basic dXNlcjpwYXNz"),
			wantCode: codes.Unauthenticated,
		},
		{
			name:     "issuer unavailable",
			ctx:      ctxWithCert(nil, "authorization", "Bearer "+downToken),
			wantCode: codes.Unavailable,
		},
		{
			name:     "no credentials",
			ctx:      ctxWithCert(nil),
			wantKind: KindUnauthenticated,
		},
		{
			name:     "unclassified cert and no credentials",
			ctx:      ctxWithCert(otherLeaf),
			wantKind: KindUnauthenticated,
		},
		{
			name:     "system cert with unlisted DNS name",
			ctx:      ctxWithCert(unlistedLeaf),
			wantKind: KindUnauthenticated,
		},
		{
			name:     "system cert with only CommonName",
			ctx:      ctxWithCert(cnOnlyLeaf),
			wantKind: KindUnauthenticated,
		},
		{
			name:     "no peer info",
			ctx:      context.Background(),
			wantKind: KindUnauthenticated,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got *PrincipalInfo
			handler := func(ctx context.Context, req any) (any, error) {
				got = FromContext(ctx)
				return "ok", nil
			}

			resp, err := a.UnaryServerInterceptor(tt.ctx, "req", &grpc.UnaryServerInfo{FullMethod: "/test/Method"}, handler)
			if tt.wantCode != codes.OK {
				if err == nil {
					t.Fatalf("interceptor succeeded, want code %v", tt.wantCode)
				}
				if status.Code(err) != tt.wantCode {
					t.Fatalf("code = %v, want %v", status.Code(err), tt.wantCode)
				}
				if got != nil {
					t.Fatalf("handler was invoked despite rejection")
				}
				return
			}
			if err != nil {
				t.Fatalf("interceptor returned error: %v", err)
			}
			if resp != "ok" {
				t.Fatalf("unexpected response: %v", resp)
			}
			if got == nil {
				t.Fatalf("no principal in context")
			}
			if got.Kind != tt.wantKind {
				t.Errorf("Kind = %q, want %q", got.Kind, tt.wantKind)
			}
			if tt.wantID != "" && got.ID != tt.wantID {
				t.Errorf("ID = %q, want %q", got.ID, tt.wantID)
			}
			if tt.wantForwarder != "" {
				if got.Forwarder == nil {
					t.Fatalf("Forwarder = nil, want %q", tt.wantForwarder)
				}
				if got.Forwarder.ID != tt.wantForwarder {
					t.Errorf("Forwarder.ID = %q, want %q", got.Forwarder.ID, tt.wantForwarder)
				}
				if got.Forwarder.Kind != KindSystem {
					t.Errorf("Forwarder.Kind = %q, want %q", got.Forwarder.Kind, KindSystem)
				}
			} else if got.Forwarder != nil {
				t.Errorf("Forwarder = %+v, want nil", got.Forwarder)
			}
		})
	}

	t.Run("forwarded token claims", func(t *testing.T) {
		ctx := ctxWithCert(routerLeaf, atemetadata.ForwardedJWTKey, actorToken)
		handler := func(ctx context.Context, req any) (any, error) {
			p := FromContext(ctx)
			if p.Token == nil {
				t.Fatalf("Token = nil, want verified token")
			}
			substrate, err := p.Token.Substrate()
			if err != nil {
				t.Fatalf("Substrate: %v", err)
			}
			if substrate.AppID != "app1" || substrate.UserID != "user1" || substrate.SessionID != "session1" {
				t.Errorf("Substrate = %+v, want app1/user1/session1", substrate)
			}
			return "ok", nil
		}
		if _, err := a.UnaryServerInterceptor(ctx, "req", &grpc.UnaryServerInfo{FullMethod: "/test/Method"}, handler); err != nil {
			t.Fatalf("interceptor: %v", err)
		}
	})
}

// fakeServerStream carries a context for stream interceptor tests.
type fakeServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (f *fakeServerStream) Context() context.Context { return f.ctx }

func TestStreamServerInterceptor(t *testing.T) {
	systemCA, err := localca.GenerateED25519CA("system-ca")
	if err != nil {
		t.Fatalf("generate system CA: %v", err)
	}
	apiLeaf := signLeaf(t, systemCA, &x509.Certificate{
		DNSNames:    []string{apiName},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})

	a := New(Config{
		SystemRoots: rootPool(systemCA),
		SystemNames: []string{apiName},
	})

	t.Run("system cert", func(t *testing.T) {
		var got *PrincipalInfo
		handler := func(srv any, ss grpc.ServerStream) error {
			got = FromContext(ss.Context())
			return nil
		}
		ss := &fakeServerStream{ctx: ctxWithCert(apiLeaf)}
		if err := a.StreamServerInterceptor(nil, ss, &grpc.StreamServerInfo{FullMethod: "/test/Stream"}, handler); err != nil {
			t.Fatalf("interceptor: %v", err)
		}
		if got == nil {
			t.Fatalf("no principal in stream context")
		}
		if got.Kind != KindSystem || got.ID != apiName {
			t.Errorf("principal = %q/%q, want %q/%q", got.Kind, got.ID, KindSystem, apiName)
		}
	})

	t.Run("invalid bearer rejected", func(t *testing.T) {
		handler := func(srv any, ss grpc.ServerStream) error {
			t.Fatalf("handler invoked despite rejection")
			return nil
		}
		ss := &fakeServerStream{ctx: ctxWithCert(nil, "authorization", "Bearer garbage")}
		err := a.StreamServerInterceptor(nil, ss, &grpc.StreamServerInfo{FullMethod: "/test/Stream"}, handler)
		if status.Code(err) != codes.Unauthenticated {
			t.Fatalf("code = %v, want %v", status.Code(err), codes.Unauthenticated)
		}
	})
}

func TestFromContextDefault(t *testing.T) {
	p := FromContext(context.Background())
	if p == nil {
		t.Fatalf("FromContext returned nil")
	}
	if p.Kind != KindUnauthenticated {
		t.Errorf("Kind = %q, want KindUnauthenticated", p.Kind)
	}
}
