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

// Package authn provides gRPC interceptors that authenticate incoming
// requests and store the resulting PrincipalInfo in the request context for
// handlers to authorize against.
//
// Three credential sources are recognized, evaluated in precedence order:
//
//  1. A forwarded JWT (the atemetadata.ForwardedJWTKey metadata key): honored
//     only when the mTLS peer is an allowlisted forwarder (the atenet
//     router); the OIDC-verified token determines the effective principal
//     and the peer's identity is preserved as the Forwarder.
//  2. The mTLS client certificate: certificates signed by the service-dns CA
//     pool carrying an allowlisted DNS SAN identify the system.
//  3. A self-asserted bearer token (the authorization metadata key): honored
//     only when the peer presents no classifiable certificate; the
//     OIDC-verified token determines the principal.
//
// Requests with no credentials at all pass through marked Unauthenticated so
// that individual handlers decide how to respond. Credentials that are
// present but invalid are rejected: a token that fails verification, or a
// forwarded JWT from a peer that is not an allowlisted forwarder, fails the
// RPC with codes.Unauthenticated. A failure to reach an issuer's keys fails
// with codes.Unavailable instead, because the token may be valid.
package authn

import (
	"context"
	"crypto/x509"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/agent-substrate/substrate/internal/atemetadata"
	"github.com/agent-substrate/substrate/internal/oidcjwt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// Kind labels the class of principal making a request. It is an open label:
// certificate-authenticated principals always carry KindSystem, while
// JWT-authenticated principals carry whatever label their issuer's
// configuration declares. The constants below cover the well-known labels.
type Kind string

const (
	// KindUnauthenticated is the zero value: the request carried no
	// credentials at all.
	KindUnauthenticated Kind = ""
	// KindSystem identifies a client certificate signed by the service-dns CA
	// pool with an allowlisted DNS SAN. This label is fixed in code;
	// certificate classification is not configuration-driven.
	KindSystem Kind = "system"
	// KindActorJWT is the conventional label for the session-id broker issuer.
	KindActorJWT Kind = "actor-jwt"
	// KindK8sJWT is the conventional label for principals authenticated
	// by the Kubernetes cluster's own OIDC issuer (service-account tokens and
	// TokenRequest-minted client tokens).
	KindK8sJWT Kind = "k8s-jwt"
)

// PrincipalInfo represents the authenticated identity of a principal.
type PrincipalInfo struct {
	Kind Kind
	// ID is the principal's identity: the allowlisted DNS SAN for
	// certificate-authenticated principals, or the issuer's configured
	// identity claim for JWT-authenticated principals.
	ID string
	// Token carries the verified JWT for JWT-authenticated principals,
	// exposing typed claim accessors. Nil for certificate-authenticated
	// principals.
	Token *oidcjwt.VerifiedToken
	// Forwarder is the authenticated System identity of the mTLS peer that
	// forwarded this principal's JWT. It is set only for forwarded
	// identities; directly-authenticated principals carry nil.
	Forwarder *PrincipalInfo
}

type contextKey struct{}

// NewContext returns a copy of ctx carrying the given principal.
func NewContext(ctx context.Context, p *PrincipalInfo) context.Context {
	return context.WithValue(ctx, contextKey{}, p)
}

// FromContext returns the principal stored in ctx. If no principal is present
// it returns an Unauthenticated principal, so callers never receive nil.
func FromContext(ctx context.Context) *PrincipalInfo {
	if p, ok := ctx.Value(contextKey{}).(*PrincipalInfo); ok {
		return p
	}
	return &PrincipalInfo{Kind: KindUnauthenticated}
}

// Config configures an Authenticator.
type Config struct {
	// SystemRoots is the CA pool (service-dns CA) that signs System certificates.
	SystemRoots *x509.CertPool
	// SystemNames is the set of DNS SANs (e.g. "api.ate-system.svc") accepted for
	// System identities. A certificate that chains to SystemRoots but presents no
	// DNS SAN in this set is not classified into a principal. The allowlist
	// fails closed: an empty set accepts no System identity.
	SystemNames []string
	// TrustedForwarders is the set of System identities (DNS SANs, expected to
	// be a subset of SystemNames) allowed to assert forwarded JWTs. It fails
	// closed: if it is empty, no forwarded JWT is ever honored.
	TrustedForwarders []string
	// Verifier verifies JWTs against the configured OIDC issuers. A verifier
	// over an empty issuer set rejects every token (fail closed).
	Verifier *oidcjwt.Verifier
}

// Authenticator authenticates requests from client certificates and
// OIDC-verified JWTs.
type Authenticator struct {
	systemRoots       *x509.CertPool
	systemNames       map[string]struct{}
	trustedForwarders map[string]struct{}
	verifier          *oidcjwt.Verifier
}

// New returns an Authenticator for the given configuration.
func New(cfg Config) *Authenticator {
	return &Authenticator{
		systemRoots:       cfg.SystemRoots,
		systemNames:       toSet(cfg.SystemNames),
		trustedForwarders: toSet(cfg.TrustedForwarders),
		verifier:          cfg.Verifier,
	}
}

// toSet builds a lookup set from items, returning nil for an empty input so
// that membership checks against it always fail (fail closed).
func toSet(items []string) map[string]struct{} {
	if len(items) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(items))
	for _, it := range items {
		set[it] = struct{}{}
	}
	return set
}

// errUnauthenticated is the generic rejection returned to clients; the
// underlying cause is logged but not leaked.
var errUnauthenticated = status.Error(codes.Unauthenticated, "unauthenticated")

// UnaryServerInterceptor authenticates the request, stores the resulting
// PrincipalInfo in the context, and invokes the handler. Requests carrying
// invalid credentials are rejected; requests carrying none pass through
// marked Unauthenticated.
func (a *Authenticator) UnaryServerInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	principal, err := a.authenticate(ctx)
	if err != nil {
		return nil, err
	}
	return handler(NewContext(ctx, principal), req)
}

// wrappedStream overrides the stream's context to carry the principal.
type wrappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedStream) Context() context.Context {
	return w.ctx
}

// StreamServerInterceptor is the streaming counterpart of
// UnaryServerInterceptor.
func (a *Authenticator) StreamServerInterceptor(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	principal, err := a.authenticate(ss.Context())
	if err != nil {
		return err
	}
	return handler(srv, &wrappedStream{ServerStream: ss, ctx: NewContext(ss.Context(), principal)})
}

// authenticate evaluates the request's credentials in precedence order:
// forwarded JWT, then client certificate, then self-asserted bearer token.
// The first matching rule wins. It returns an error for credentials that are
// present but invalid, and an Unauthenticated principal when no credentials
// are present.
func (a *Authenticator) authenticate(ctx context.Context) (*PrincipalInfo, error) {
	peerPrincipal := a.classifyPeerCertificate(ctx)
	md, _ := metadata.FromIncomingContext(ctx)

	// 1. A forwarded JWT is honored only from an allowlisted forwarder. Its
	// presence on any other connection is anomalous (well-behaved clients
	// never set it), so it is rejected rather than ignored.
	if forwarded := md.Get(atemetadata.ForwardedJWTKey); len(forwarded) > 0 {
		if peerPrincipal == nil || peerPrincipal.Kind != KindSystem {
			slog.WarnContext(ctx, "Rejecting forwarded JWT from peer without a system identity")
			return nil, errUnauthenticated
		}
		if _, ok := a.trustedForwarders[peerPrincipal.ID]; !ok {
			slog.WarnContext(ctx, "Rejecting forwarded JWT from peer that is not a trusted forwarder", slog.String("peer", peerPrincipal.ID))
			return nil, errUnauthenticated
		}
		if len(forwarded) != 1 {
			slog.WarnContext(ctx, "Rejecting request with multiple forwarded JWTs", slog.String("peer", peerPrincipal.ID))
			return nil, errUnauthenticated
		}
		token, err := a.verifyJWT(ctx, forwarded[0])
		if err != nil {
			return nil, err
		}
		return &PrincipalInfo{
			Kind:      Kind(token.Issuer.Kind),
			ID:        token.ID,
			Token:     token,
			Forwarder: peerPrincipal,
		}, nil
	}

	// 2. A classified client certificate is the principal. Any self-asserted
	// bearer token on such a connection is ignored outright, not evaluated.
	if peerPrincipal != nil {
		return peerPrincipal, nil
	}

	// 3. A self-asserted bearer token from a peer with no classifiable
	// certificate.
	if authorization := md.Get("authorization"); len(authorization) > 0 {
		if len(authorization) != 1 {
			slog.WarnContext(ctx, "Rejecting request with multiple authorization headers")
			return nil, errUnauthenticated
		}
		raw, ok := strings.CutPrefix(authorization[0], "Bearer ")
		if !ok {
			slog.WarnContext(ctx, "Rejecting request with non-Bearer authorization header")
			return nil, errUnauthenticated
		}
		token, err := a.verifyJWT(ctx, raw)
		if err != nil {
			return nil, err
		}
		return &PrincipalInfo{
			Kind:  Kind(token.Issuer.Kind),
			ID:    token.ID,
			Token: token,
		}, nil
	}

	// 4. No credentials at all: pass through for handlers to decide.
	return &PrincipalInfo{Kind: KindUnauthenticated}, nil
}

// verifyJWT verifies token, mapping verification failures to a generic
// Unauthenticated rejection and key-fetch failures to Unavailable: when the
// issuer's keys cannot be reached the token may be perfectly valid, so the
// client should retry rather than treat the credential as rejected.
func (a *Authenticator) verifyJWT(ctx context.Context, token string) (*oidcjwt.VerifiedToken, error) {
	if a.verifier == nil {
		slog.WarnContext(ctx, "Rejecting JWT: no OIDC issuers are configured")
		return nil, errUnauthenticated
	}
	verified, err := a.verifier.Verify(ctx, token, time.Now())
	if err != nil {
		if errors.Is(err, oidcjwt.ErrUnavailable) {
			slog.ErrorContext(ctx, "Failed to fetch issuer keys while verifying JWT", slog.Any("err", err))
			return nil, status.Error(codes.Unavailable, "issuer keys unavailable")
		}
		slog.WarnContext(ctx, "Rejecting invalid JWT", slog.Any("err", err))
		return nil, errUnauthenticated
	}
	return verified, nil
}

// classifyPeerCertificate classifies the peer's client certificate, returning
// a System principal for certificates signed by the service-dns CA pool with
// an allowlisted DNS SAN, and nil for everything else. Unclassified
// certificates (e.g. workerpool client certificates) are accepted at the TLS
// layer but carry no principal.
func (a *Authenticator) classifyPeerCertificate(ctx context.Context) *PrincipalInfo {
	leaf := peerLeafCertificate(ctx)
	if leaf == nil {
		return nil
	}
	if a.systemRoots == nil || !chainsTo(leaf, a.systemRoots) {
		return nil
	}
	id, ok := a.systemID(leaf)
	if !ok {
		return nil
	}
	return &PrincipalInfo{Kind: KindSystem, ID: id}
}

// peerLeafCertificate returns the leaf certificate the peer presented during
// the mTLS handshake, or nil if none is available.
func peerLeafCertificate(ctx context.Context) *x509.Certificate {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return nil
	}
	if len(tlsInfo.State.PeerCertificates) == 0 {
		return nil
	}
	return tlsInfo.State.PeerCertificates[0]
}

// chainsTo reports whether leaf verifies against the given roots as a client
// certificate.
func chainsTo(leaf *x509.Certificate, roots *x509.CertPool) bool {
	_, err := leaf.Verify(x509.VerifyOptions{
		Roots:     roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	return err == nil
}

// systemID extracts the identity of a system principal from its certificate.
// Service certs carry DNS SANs (e.g. api.ate-system.svc). It returns the first
// DNS SAN present in the allowlist and true; certificates whose DNS SANs are all
// unrecognized are rejected. Matching against the allowlist rather than reading
// DNSNames[0] avoids trusting SAN ordering, and there is no CommonName fallback.
func (a *Authenticator) systemID(leaf *x509.Certificate) (string, bool) {
	for _, name := range leaf.DNSNames {
		if _, ok := a.systemNames[name]; ok {
			return name, true
		}
	}
	return "", false
}
