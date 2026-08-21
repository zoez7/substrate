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

package ategcs

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/agent-substrate/substrate/internal/ateerrors"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type s3Client struct {
	client *s3.Client
}

func NewS3Client(client *s3.Client) ObjectStorage {
	return &s3Client{client: client}
}

func (s *s3Client) GetObject(ctx context.Context, bucket, object string) (io.ReadCloser, error) {
	output, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(object),
	})
	if err != nil {
		if _, ok := errors.AsType[*s3types.NoSuchKey](err); ok {
			return nil, fmt.Errorf("%w: Failed to get S3 Bucket:%q, Object:%q", ateerrors.ReasonFailedGetExternalObject, bucket, object)
		}
		return nil, classifyS3Err(fmt.Errorf("while reading S3 Bucket:%q, Object:%q: %w", bucket, object, err))
	}
	return output.Body, nil
}

func (s *s3Client) PutObject(ctx context.Context, bucket, object string, reader io.Reader) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(object),
		Body:   reader,
	})
	if err != nil {
		return classifyS3Err(fmt.Errorf("while putting S3 object: %w", err))
	}
	return nil
}

// classifyS3Err tags err transient iff the AWS SDK's own standard retryer
// would retry it (connection trouble, throttling, retryable status codes).
// Deterministic failures (AccessDenied, malformed requests, ...) stay
// untagged and crash the actor by default.
func classifyS3Err(err error) error {
	for _, r := range retry.DefaultRetryables {
		if r.IsErrorRetryable(err) == aws.TrueTernary {
			return fmt.Errorf("%w: %w", ateerrors.ReasonObjectStorageUnavailable, err)
		}
	}
	return err
}
