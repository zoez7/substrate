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

// Package atemetadata defines the gRPC metadata keys shared between
// substrate components.
package atemetadata

// ForwardedJWTKey is the metadata key under which the atenet router forwards
// the originating actor's session JWT when calling the ate-apiserver on the
// actor's behalf. The apiserver honors it only when the mTLS peer is an
// allowlisted forwarder; it must never be set by other clients.
const ForwardedJWTKey = "x-substrate-forwarded-jwt"
