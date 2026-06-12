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

// Package oidcjwt verifies JWTs against a configured set of OIDC issuers.
//
// Each trusted issuer is declared in a Config entry that names the issuer
// URL, the principal kind its tokens map to, the claim that identifies the
// principal, and the audiences the verifier accepts. Verification keys are
// discovered through the issuer's OIDC discovery document and JWKS endpoint.
// Tokens from issuers that are not configured are rejected (fail closed).
//
// The verifier distinguishes two failure classes: a token that fails
// verification (bad signature, expired, unknown issuer, wrong audience) is
// invalid, while a failure to reach the issuer's discovery or JWKS endpoint
// is reported with ErrUnavailable so that callers can surface a retryable
// error instead of treating a possibly-valid token as unauthenticated.
package oidcjwt

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"time"
)

// ErrUnavailable indicates that the issuer's discovery or JWKS endpoint could
// not be reached or returned an unusable response. The token under
// verification may be perfectly valid; callers should map this to a retryable
// error rather than rejecting the credential.
var ErrUnavailable = errors.New("issuer keys unavailable")

var permittedSkew = 5 * time.Minute

// fetchTimeout bounds each discovery/JWKS HTTP request.
const fetchTimeout = 10 * time.Second

// IssuerConfig declares one trusted OIDC issuer and how its tokens map to a
// principal.
type IssuerConfig struct {
	// Issuer is the expected `iss` claim. Verification keys are discovered
	// from Issuer + "/.well-known/openid-configuration".
	Issuer string
	// Kind is the principal kind label assigned to tokens from this issuer
	// (e.g. "actor-jwt", "k8s-jwt"). It is an open label interpreted by callers.
	Kind string
	// IDClaim is the verified claim whose value becomes the principal's
	// identity. Defaults to "sub" when unset in the serialized config.
	IDClaim string
	// Audiences is the allowlist of accepted audiences. A token must carry at
	// least one of these in its `aud` claim. It is never empty (fail closed).
	Audiences []string
}

// Config is the set of trusted issuers.
type Config struct {
	Issuers []*IssuerConfig
}

type serializedConfig struct {
	Issuers []*serializedIssuer `json:"issuers"`
}

type serializedIssuer struct {
	Issuer    string   `json:"issuer"`
	Kind      string   `json:"kind"`
	IDClaim   string   `json:"idClaim,omitempty"`
	Audiences []string `json:"audiences"`
}

// Unmarshal parses and validates a serialized issuer configuration. Every
// entry must name an issuer, a kind, and at least one audience; entries
// without audiences are rejected rather than defaulted so the config fails
// closed.
func Unmarshal(wireBytes []byte) (*Config, error) {
	var wire serializedConfig
	if err := json.Unmarshal(wireBytes, &wire); err != nil {
		return nil, fmt.Errorf("while unmarshaling OIDC config: %w", err)
	}

	cfg := &Config{}
	seen := make(map[string]struct{}, len(wire.Issuers))
	for i, wi := range wire.Issuers {
		if wi == nil {
			return nil, fmt.Errorf("issuer entry %d is null", i)
		}
		if wi.Issuer == "" {
			return nil, fmt.Errorf("issuer entry %d: issuer URL is required", i)
		}
		if _, ok := seen[wi.Issuer]; ok {
			return nil, fmt.Errorf("issuer entry %d: duplicate issuer %q", i, wi.Issuer)
		}
		seen[wi.Issuer] = struct{}{}
		if wi.Kind == "" {
			return nil, fmt.Errorf("issuer entry %d (%s): kind is required", i, wi.Issuer)
		}
		if len(wi.Audiences) == 0 {
			return nil, fmt.Errorf("issuer entry %d (%s): at least one audience is required", i, wi.Issuer)
		}
		idClaim := wi.IDClaim
		if idClaim == "" {
			idClaim = "sub"
		}
		cfg.Issuers = append(cfg.Issuers, &IssuerConfig{
			Issuer:    wi.Issuer,
			Kind:      wi.Kind,
			IDClaim:   idClaim,
			Audiences: wi.Audiences,
		})
	}
	return cfg, nil
}

// Options configures a Verifier.
type Options struct {
	// RootCAs, if set, replaces the system roots when verifying the TLS
	// certificates of issuer discovery and JWKS endpoints. Set this to the
	// service-dns trust bundle so in-cluster issuers (e.g. the session-id
	// broker) can be reached over TLS.
	RootCAs *x509.CertPool
}

// Verifier verifies JWTs against the configured issuers.
type Verifier struct {
	issuers map[string]*IssuerConfig
	client  *http.Client
}

// NewVerifier returns a Verifier trusting exactly the issuers in cfg.
func NewVerifier(cfg *Config, opts Options) *Verifier {
	issuers := make(map[string]*IssuerConfig, len(cfg.Issuers))
	for _, ic := range cfg.Issuers {
		issuers[ic.Issuer] = ic
	}
	transport := http.DefaultTransport
	if opts.RootCAs != nil {
		transport = &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: opts.RootCAs, MinVersion: tls.VersionTLS12},
		}
	}
	return &Verifier{
		issuers: issuers,
		client:  &http.Client{Timeout: fetchTimeout, Transport: transport},
	}
}

// VerifiedToken is the result of successfully verifying a JWT: its issuer's
// configuration, the extracted identity, and the standard claims. The full
// verified payload backs the typed claim accessors.
type VerifiedToken struct {
	// Issuer is the configuration entry of the issuer that signed the token.
	Issuer *IssuerConfig
	// ID is the value of the issuer's IDClaim.
	ID string

	// Claims from RFC7519.
	Subject    string
	Audiences  []string
	Expiration time.Time
	NotBefore  time.Time
	IssuedAt   time.Time
	JTI        string

	payload []byte
}

type parseHeader struct {
	Type      string `json:"typ,omitempty"`
	Algorithm string `json:"alg,omitempty"`
	KeyID     string `json:"kid,omitempty"`
}

type parseClaims struct {
	Issuer     string          `json:"iss,omitempty"`
	Subject    string          `json:"sub,omitempty"`
	Audiences  json.RawMessage `json:"aud,omitempty"`
	Expiration float64         `json:"exp,omitempty"`
	NotBefore  float64         `json:"nbf,omitempty"`
	IssuedAt   float64         `json:"iat,omitempty"`
	JTI        string          `json:"jti,omitempty"`
}

// Verify verifies token against the configured issuers: it discovers the
// issuer's keys, checks the signature, and checks the issuer, audience, and
// time bindings. Tokens from unconfigured issuers fail verification. A
// failure to fetch the issuer's keys is reported with ErrUnavailable.
func (v *Verifier) Verify(ctx context.Context, token string, now time.Time) (*VerifiedToken, error) {
	segments := strings.Split(token, ".")
	if len(segments) != 3 {
		return nil, fmt.Errorf("malformed JWT")
	}
	headerB64String := segments[0]
	payloadB64String := segments[1]
	signatureB64String := segments[2]

	headerBytes, err := base64.RawURLEncoding.DecodeString(headerB64String)
	if err != nil {
		return nil, fmt.Errorf("while base64 decoding header: %w", err)
	}

	signatureBytes, err := base64.RawURLEncoding.DecodeString(signatureB64String)
	if err != nil {
		return nil, fmt.Errorf("while base64 decoding signature: %w", err)
	}

	var header parseHeader
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return nil, fmt.Errorf("while unmarshaling header: %w", err)
	}

	// Not all issuers set the `typ` header field, but if present it must be
	// the spec-recommended value.
	switch header.Type {
	case "", "JWT": // OK
	default:
		return nil, fmt.Errorf("unexpected value in type header")
	}

	// Parse the payload. The payload is not verified at this point, so the
	// only safe thing to do with it is extract the issuer, check the issuer
	// against the configured set, and fetch keys from the issuer.
	//
	// Don't consider any other data in the payload until the call to
	// verifySignature() below.
	payloadBytes, err := base64.RawURLEncoding.DecodeString(payloadB64String)
	if err != nil {
		return nil, fmt.Errorf("while base64-decoding payload: %w", err)
	}
	var rawClaims parseClaims
	if err := json.Unmarshal(payloadBytes, &rawClaims); err != nil {
		return nil, fmt.Errorf("while unmarshaling payload: %w", err)
	}

	ic, ok := v.issuers[rawClaims.Issuer]
	if !ok {
		return nil, fmt.Errorf("issuer %q is not configured", rawClaims.Issuer)
	}

	// TODO: Cache keys, and only fetch new keys if the JWT's key ID is not in
	// the cache.
	keys, err := v.discoverKeysForIssuer(ctx, ic.Issuer)
	if err != nil {
		return nil, fmt.Errorf("while discovering keys from issuer: %w", err)
	}

	// Find the key we should use for verification based on the key ID in the
	// JWT header.
	if header.KeyID == "" {
		return nil, fmt.Errorf("key ID is required")
	}
	selectedKeyIndex := slices.IndexFunc(keys, func(k *keyAndID) bool {
		return k.KeyID == header.KeyID
	})
	if selectedKeyIndex == -1 {
		return nil, fmt.Errorf("unknown key ID %q", header.KeyID)
	}
	selectedKey := keys[selectedKeyIndex].PublicKey

	// Warning: don't ever refer to the payload data (except "iss") above this
	// point. We need to ensure that we _never_ consider the contents of the
	// payload when deciding how to perform signature verification.
	if err := verifySignature(header.Algorithm, selectedKey, []byte(headerB64String+"."+payloadB64String), signatureBytes); err != nil {
		return nil, fmt.Errorf("while verifying JWT signature: %w", err)
	}

	// It is now safe to consider arbitrary data from the payload.
	//
	// At this point, the payload is mostly trusted. We know that it was really
	// issued by the selected verification key, but we need to check the
	// audience binding and time bindings to be sure that it's really valid.

	// Because the JWT spec authors wanted to be fancy, we need to try to
	// deserialize rawClaims.Audiences both as a single string and as a slice
	// of strings.
	var singleAudience string
	var audiences []string
	if err := json.Unmarshal(rawClaims.Audiences, &singleAudience); err == nil { // err EQUALS nil
		audiences = []string{singleAudience}
	} else if err := json.Unmarshal(rawClaims.Audiences, &audiences); err == nil { // err EQUALS nil
	} else {
		return nil, fmt.Errorf("unable to parse audiences")
	}

	// Check that the token carries at least one of the issuer's accepted
	// audiences.
	accepted := slices.ContainsFunc(ic.Audiences, func(want string) bool {
		return slices.Contains(audiences, want)
	})
	if !accepted {
		return nil, fmt.Errorf("token is not issued for an accepted audience")
	}

	expiration := time.Unix(int64(rawClaims.Expiration), 0)
	notBefore := time.Unix(int64(rawClaims.NotBefore), 0)
	issuedAt := time.Unix(int64(rawClaims.IssuedAt), 0)

	if expiration.Before(now.Add(-permittedSkew)) {
		return nil, fmt.Errorf("jwt has expired")
	}

	if notBefore.After(now.Add(permittedSkew)) {
		return nil, fmt.Errorf("jwt is not valid yet")
	}

	if issuedAt.After(now.Add(permittedSkew)) {
		return nil, fmt.Errorf("jwt claims to have been issued in the future")
	}

	// Extract the identity from the configured claim.
	var claimMap map[string]any
	if err := json.Unmarshal(payloadBytes, &claimMap); err != nil {
		return nil, fmt.Errorf("while unmarshaling payload claims: %w", err)
	}
	id, _ := claimMap[ic.IDClaim].(string)
	if id == "" {
		return nil, fmt.Errorf("token carries no usable %q identity claim", ic.IDClaim)
	}

	return &VerifiedToken{
		Issuer:     ic,
		ID:         id,
		Subject:    rawClaims.Subject,
		Audiences:  audiences,
		Expiration: expiration,
		NotBefore:  notBefore,
		IssuedAt:   issuedAt,
		JTI:        rawClaims.JTI,
		payload:    payloadBytes,
	}, nil
}

// keyAndID wraps a crypto.PublicKey along with the key ID that identifies it
// during the verification process.
type keyAndID struct {
	KeyID     string
	PublicKey crypto.PublicKey
}

type oidcConfigT struct {
	JWKSURI string `json:"jwks_uri"`
}

type jwkSetT struct {
	Keys []jwkT `json:"keys"`
}

type jwkT struct {
	KeyType string `json:"kty"`
	KeyID   string `json:"kid,omitempty"`

	EllipticCurve string `json:"crv,omitempty"`
	EllipticX     string `json:"x,omitempty"`
	EllipticY     string `json:"y,omitempty"`

	RSAN string `json:"n"`
	RSAE string `json:"e"`
}

func (v *Verifier) discoverKeysForIssuer(ctx context.Context, issuer string) ([]*keyAndID, error) {
	var discoveryDocURL string
	if strings.HasSuffix(issuer, "/") {
		discoveryDocURL = issuer + ".well-known/openid-configuration"
	} else {
		discoveryDocURL = issuer + "/.well-known/openid-configuration"
	}

	oidcConfig, err := fetchJSON[oidcConfigT](ctx, v.client, discoveryDocURL)
	if err != nil {
		return nil, fmt.Errorf("while fetching OIDC Discovery document: %w", err)
	}

	slog.DebugContext(ctx, "Fetched discovery doc", slog.Any("doc", oidcConfig))

	jwkSet, err := fetchJSON[jwkSetT](ctx, v.client, oidcConfig.JWKSURI)
	if err != nil {
		return nil, fmt.Errorf("while fetching JWKS: %w", err)
	}

	slog.DebugContext(ctx, "Fetched JWK set", slog.Any("jwkSet", jwkSet))

	var ret []*keyAndID
	for _, jwk := range jwkSet.Keys {
		if jwk.KeyID == "" {
			return nil, fmt.Errorf("JWKs endpoint returned key without key ID")
		}

		switch jwk.KeyType {
		case "EC":
			var curve elliptic.Curve
			switch jwk.EllipticCurve {
			case "P-256":
				curve = elliptic.P256()
			case "P-384":
				curve = elliptic.P384()
			case "P-521":
				curve = elliptic.P521()
			default:
				return nil, fmt.Errorf("unhandled elliptic curve %q", jwk.EllipticCurve)
			}

			xBytes, err := base64.RawURLEncoding.DecodeString(jwk.EllipticX)
			if err != nil {
				return nil, fmt.Errorf("while base64-decoding x: %w", err)
			}
			yBytes, err := base64.RawURLEncoding.DecodeString(jwk.EllipticY)
			if err != nil {
				return nil, fmt.Errorf("while base64-decoding y: %w", err)
			}

			ret = append(ret, &keyAndID{
				KeyID: jwk.KeyID,
				PublicKey: &ecdsa.PublicKey{
					Curve: curve,
					X:     big.NewInt(0).SetBytes(xBytes),
					Y:     big.NewInt(0).SetBytes(yBytes),
				},
			})

		case "RSA":
			nBytes, err := base64.RawURLEncoding.DecodeString(jwk.RSAN)
			if err != nil {
				return nil, fmt.Errorf("while base64-decoding n: %w", err)
			}
			n := &big.Int{}
			n.SetBytes(nBytes)

			eBytes, err := base64.RawURLEncoding.DecodeString(jwk.RSAE)
			if err != nil {
				return nil, fmt.Errorf("while base64-decoding e: %w", err)
			}
			e := &big.Int{}
			e.SetBytes(eBytes)

			ret = append(ret, &keyAndID{
				KeyID: jwk.KeyID,
				PublicKey: &rsa.PublicKey{
					N: n,
					E: int(e.Int64()),
				},
			})

		default:
			return nil, fmt.Errorf("unhandled key type %q", jwk.KeyType)
		}
	}

	return ret, nil
}

func verifySignature(algorithm string, selectedKey crypto.PublicKey, toBeSignedBytes, signatureBytes []byte) error {
	switch algorithm {
	case "RS256":
		rsaKey, ok := selectedKey.(*rsa.PublicKey)
		if !ok {
			return fmt.Errorf("requested key ID is not an RSA key")
		}
		toBeSignedDigest := hashBytes(crypto.SHA256.New(), toBeSignedBytes)
		if err := rsa.VerifyPKCS1v15(rsaKey, crypto.SHA256, toBeSignedDigest, signatureBytes); err != nil {
			return fmt.Errorf("while validating RSA PKCS1v15 signature: %w", err)
		}
	case "RS384":
		rsaKey, ok := selectedKey.(*rsa.PublicKey)
		if !ok {
			return fmt.Errorf("requested key ID is not an RSA key")
		}
		toBeSignedDigest := hashBytes(crypto.SHA384.New(), toBeSignedBytes)
		if err := rsa.VerifyPKCS1v15(rsaKey, crypto.SHA384, toBeSignedDigest, signatureBytes); err != nil {
			return fmt.Errorf("while validating RSA PKCS1v15 signature: %w", err)
		}
	case "RS512":
		rsaKey, ok := selectedKey.(*rsa.PublicKey)
		if !ok {
			return fmt.Errorf("requested key ID is not an RSA key")
		}
		toBeSignedDigest := hashBytes(crypto.SHA512.New(), toBeSignedBytes)
		if err := rsa.VerifyPKCS1v15(rsaKey, crypto.SHA512, toBeSignedDigest, signatureBytes); err != nil {
			return fmt.Errorf("while validating RSA PKCS1v15 signature: %w", err)
		}
	case "ES256":
		ecdsaKey, ok := selectedKey.(*ecdsa.PublicKey)
		if !ok || ecdsaKey.Curve != elliptic.P256() {
			return fmt.Errorf("requested key ID is not an ECDSA P256 key")
		}
		toBeSignedDigest := hashBytes(crypto.SHA256.New(), toBeSignedBytes)
		if len(signatureBytes) != 2*32 {
			return fmt.Errorf("invalid ecdsa signature")
		}
		r := big.NewInt(0).SetBytes(signatureBytes[:32])
		s := big.NewInt(0).SetBytes(signatureBytes[32:])
		if !ecdsa.Verify(ecdsaKey, toBeSignedDigest, r, s) {
			return fmt.Errorf("invalid ecdsa signature")
		}
	case "ES384":
		ecdsaKey, ok := selectedKey.(*ecdsa.PublicKey)
		if !ok || ecdsaKey.Curve != elliptic.P384() {
			return fmt.Errorf("requested key ID is not an ECDSA P384 key")
		}
		toBeSignedDigest := hashBytes(crypto.SHA384.New(), toBeSignedBytes)
		if len(signatureBytes) != 2*48 {
			return fmt.Errorf("invalid ecdsa signature")
		}
		r := big.NewInt(0).SetBytes(signatureBytes[:48])
		s := big.NewInt(0).SetBytes(signatureBytes[48:])
		if !ecdsa.Verify(ecdsaKey, toBeSignedDigest, r, s) {
			return fmt.Errorf("invalid ecdsa signature")
		}
	case "ES512":
		ecdsaKey, ok := selectedKey.(*ecdsa.PublicKey)
		if !ok || ecdsaKey.Curve != elliptic.P521() {
			return fmt.Errorf("requested key ID is not an ECDSA P521 key")
		}
		toBeSignedDigest := hashBytes(crypto.SHA512.New(), toBeSignedBytes)
		if len(signatureBytes) != 2*66 {
			return fmt.Errorf("invalid ecdsa signature")
		}
		r := big.NewInt(0).SetBytes(signatureBytes[:66])
		s := big.NewInt(0).SetBytes(signatureBytes[66:])
		if !ecdsa.Verify(ecdsaKey, toBeSignedDigest, r, s) {
			return fmt.Errorf("invalid ecdsa signature")
		}
	default:
		return fmt.Errorf("unsupported algorithm %q", algorithm)
	}

	return nil
}

func hashBytes(hasher hash.Hash, bytes []byte) []byte {
	hasher.Write(bytes)
	hash := hasher.Sum(nil)
	return hash[:]
}

func fetchJSON[T any](ctx context.Context, client *http.Client, url string) (T, error) {
	var parsedBody T

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return parsedBody, fmt.Errorf("while building HTTP request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return parsedBody, fmt.Errorf("%w: while making HTTP request: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return parsedBody, fmt.Errorf("%w: non-200 response code %d", ErrUnavailable, resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return parsedBody, fmt.Errorf("%w: while reading response body: %v", ErrUnavailable, err)
	}

	if err := json.Unmarshal(bodyBytes, &parsedBody); err != nil {
		return parsedBody, fmt.Errorf("%w: while parsing response body: %v", ErrUnavailable, err)
	}

	return parsedBody, nil
}
