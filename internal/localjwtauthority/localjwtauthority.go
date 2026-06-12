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

// Package localjwtauthority implements a simple "CA" for JWTs.
package localjwtauthority

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
)

type Pool struct {
	Authorities []*Authority
}

type Authority struct {
	ID         string
	Algorithm  string
	SigningKey crypto.PrivateKey
}

type serializedPool struct {
	Authorities []*serializedAuthority
}

type serializedAuthority struct {
	ID              string
	Algorithm       string
	SigningKeyPKCS8 []byte
}

// Marshal serializes a Pool to JSON.
func Marshal(pool *Pool) ([]byte, error) {
	wire := &serializedPool{}

	for _, authority := range pool.Authorities {
		authorityWire := &serializedAuthority{}
		authorityWire.ID = authority.ID
		authorityWire.Algorithm = authority.Algorithm

		signingKeyPKCS8, err := x509.MarshalPKCS8PrivateKey(authority.SigningKey)
		if err != nil {
			return nil, fmt.Errorf("while serializing signing key to PKCS#8: %w", err)
		}
		authorityWire.SigningKeyPKCS8 = signingKeyPKCS8

		wire.Authorities = append(wire.Authorities, authorityWire)
	}

	wireBytes, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("while marshaling to JSON: %w", err)
	}

	return wireBytes, nil
}

// Unmarshal loads a Pool from JSON.
func Unmarshal(wireBytes []byte) (*Pool, error) {
	wire := &serializedPool{}

	if err := json.Unmarshal(wireBytes, wire); err != nil {
		return nil, fmt.Errorf("while unmarshaling JSON: %w", err)
	}

	pool := &Pool{}
	for _, wireAuthority := range wire.Authorities {
		authority := &Authority{
			ID:        wireAuthority.ID,
			Algorithm: wireAuthority.Algorithm,
		}

		signingKey, err := x509.ParsePKCS8PrivateKey(wireAuthority.SigningKeyPKCS8)
		if err != nil {
			return nil, fmt.Errorf("while parsing signing key: %w", err)
		}
		authority.SigningKey = signingKey

		pool.Authorities = append(pool.Authorities, authority)
	}

	return pool, nil
}

type jwk struct {
	KeyType   string `json:"kty"`
	KeyID     string `json:"kid"`
	Use       string `json:"use"`
	Algorithm string `json:"alg"`

	EllipticCurve string `json:"crv,omitempty"`
	EllipticX     string `json:"x,omitempty"`
	EllipticY     string `json:"y,omitempty"`

	RSAN string `json:"n,omitempty"`
	RSAE string `json:"e,omitempty"`
}

type jwkSet struct {
	Keys []jwk `json:"keys"`
}

// MarshalJWKS serializes the public halves of the pool's signing keys as an
// RFC 7517 JWK Set, suitable for serving from an OIDC issuer's JWKS endpoint.
// Each key's `kid` is the authority's ID, matching the `kid` header that
// sessionidjwt.Sign places on minted tokens.
func MarshalJWKS(pool *Pool) ([]byte, error) {
	set := jwkSet{Keys: []jwk{}}
	for _, authority := range pool.Authorities {
		signer, ok := authority.SigningKey.(crypto.Signer)
		if !ok {
			return nil, fmt.Errorf("authority %q signing key does not expose a public key", authority.ID)
		}

		key := jwk{
			KeyID:     authority.ID,
			Use:       "sig",
			Algorithm: authority.Algorithm,
		}
		switch pub := signer.Public().(type) {
		case *ecdsa.PublicKey:
			var curveName string
			switch pub.Curve {
			case elliptic.P256():
				curveName = "P-256"
			case elliptic.P384():
				curveName = "P-384"
			case elliptic.P521():
				curveName = "P-521"
			default:
				return nil, fmt.Errorf("authority %q uses unhandled elliptic curve", authority.ID)
			}
			// Bytes returns the uncompressed point: 0x04 || X || Y, each
			// coordinate fixed-width.
			raw, err := pub.Bytes()
			if err != nil {
				return nil, fmt.Errorf("while encoding authority %q public key: %w", authority.ID, err)
			}
			byteLen := (pub.Curve.Params().BitSize + 7) / 8
			if len(raw) != 1+2*byteLen || raw[0] != 4 {
				return nil, fmt.Errorf("authority %q public key has unexpected encoding", authority.ID)
			}

			key.KeyType = "EC"
			key.EllipticCurve = curveName
			key.EllipticX = base64.RawURLEncoding.EncodeToString(raw[1 : 1+byteLen])
			key.EllipticY = base64.RawURLEncoding.EncodeToString(raw[1+byteLen:])

		case *rsa.PublicKey:
			e := []byte{byte(pub.E >> 16), byte(pub.E >> 8), byte(pub.E)}
			for len(e) > 1 && e[0] == 0 {
				e = e[1:]
			}
			key.KeyType = "RSA"
			key.RSAN = base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
			key.RSAE = base64.RawURLEncoding.EncodeToString(e)

		default:
			return nil, fmt.Errorf("authority %q uses unhandled key type %T", authority.ID, pub)
		}

		set.Keys = append(set.Keys, key)
	}

	wireBytes, err := json.Marshal(set)
	if err != nil {
		return nil, fmt.Errorf("while marshaling JWK set: %w", err)
	}
	return wireBytes, nil
}

// GenerateECDSAP256Authority generates an ECDSA P256 JWT signing key.
func GenerateECDSAP256Authority(id string) (*Authority, error) {
	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("while generating key: %w", err)
	}

	return &Authority{
		ID:         id,
		Algorithm:  "ES256",
		SigningKey: privKey,
	}, nil
}
