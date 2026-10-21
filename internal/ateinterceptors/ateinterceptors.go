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

package ateinterceptors

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"go.ate.dev/substrate/internal/apierror"
	"go.ate.dev/substrate/internal/principal"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// ServerElapsedTrailer carries the server's handler duration in microseconds,
// so clients can report a latency unaffected by their own scheduling overhead.
const ServerElapsedTrailer = "x-server-elapsed-us"

// ServerUnaryInterceptor is for ateapi. A handler's error reaches the caller
// with the code apierror gives it; any other error, including a status received
// from an upstream service, is Internal.
//
// Request and response bodies are logged as they are: redaction of debug_redact
// fields happens in the shared slog handler (internal/contextlogging) that
// serverboot's InitLogger and InitLoggerWithWriter install as the default logger
// in every server, so it also covers protos logged anywhere else in the process.
func ServerUnaryInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	startTime := time.Now()

	resp, err := handler(ctx, req)

	elapsed := time.Since(startTime)

	// Observability trailer; failure here must not affect the RPC outcome.
	_ = grpc.SetTrailer(ctx, metadata.Pairs(
		ServerElapsedTrailer,
		strconv.FormatInt(elapsed.Microseconds(), 10),
	))

	pInfo, _ := principal.FromContext(ctx)

	slog.InfoContext(ctx, "Handle RPC",
		slog.String("method", info.FullMethod),
		slog.Any("req", req),
		slog.Any("resp", respForLog(info.FullMethod, resp, err)),
		slog.Any("err", err),
		slog.String("elapsed-time", elapsed.String()),
		slog.Any("principal", pInfo),
	)

	if err != nil {
		if st, ok := apierror.FromError(err); ok {
			return nil, st.Err()
		}
		return nil, status.Errorf(codes.Internal, "internal server error: %v", err)
	}

	return resp, err
}

// MaxDeadlineUnaryInterceptor returns an interceptor that caps the request context at maxDeadline.
func MaxDeadlineUnaryInterceptor(maxDeadline time.Duration) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx, cancel := context.WithTimeout(ctx, maxDeadline)
		defer cancel()
		return handler(ctx, req)
	}
}

// InternalServerUnaryInterceptor is for internal services. A handler's error
// reaches the caller with the code apierror gives it; any other error, including
// a status received from an upstream service, is Internal with its full text.
func InternalServerUnaryInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	startTime := time.Now()

	resp, err := handler(ctx, req)

	elapsed := time.Since(startTime)

	_ = grpc.SetTrailer(ctx, metadata.Pairs(
		ServerElapsedTrailer,
		strconv.FormatInt(elapsed.Microseconds(), 10),
	))

	slog.InfoContext(ctx, "Handle RPC",
		slog.String("method", info.FullMethod),
		slog.Any("req", req),
		slog.Any("resp", respForLog(info.FullMethod, resp, err)),
		slog.Any("err", err),
		slog.String("elapsed-time", elapsed.String()),
	)

	if err != nil {
		st, _ := apierror.FromError(err)
		return nil, st.Err()
	}

	return resp, err
}

// respForLog keeps list responses out of the log. A page of a thousand actors
// is tens of kilobytes per call, and at scale those pages rotate the lifecycle
// lines out of the container log. A failed call's response is logged as the
// handler returned it: the generated handlers return a typed nil, which logs
// as null.
func respForLog(method string, resp any, err error) any {
	if err != nil {
		return resp
	}
	name := method[strings.LastIndex(method, "/")+1:]
	if !strings.HasPrefix(name, "List") {
		return resp
	}
	if m, ok := resp.(proto.Message); ok {
		return fmt.Sprintf("<%s response elided, %d bytes>", name, proto.Size(m))
	}
	return resp
}

// LoggingServerUnaryInterceptor logs the request, response, error, and elapsed
// time of an RPC, and returns the handler's error unchanged. Use this for
// services whose handlers already return gRPC status errors directly.
func LoggingServerUnaryInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	startTime := time.Now()

	resp, err := handler(ctx, req)

	elapsed := time.Since(startTime)

	_ = grpc.SetTrailer(ctx, metadata.Pairs(
		ServerElapsedTrailer,
		strconv.FormatInt(elapsed.Microseconds(), 10),
	))

	slog.InfoContext(ctx, "Handle RPC",
		slog.String("method", info.FullMethod),
		slog.Any("req", req),
		slog.Any("resp", respForLog(info.FullMethod, resp, err)),
		slog.Any("err", err),
		slog.String("elapsed-time", elapsed.String()),
	)

	return resp, err
}

// ClientUnaryInterceptor logs outbound unary RPC calls with the method, target,
// request, response, error, status code, elapsed time, and server elapsed time
// when reported via ServerElapsedTrailer.
func ClientUnaryInterceptor(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	startTime := time.Now()

	var trailer metadata.MD
	callOpts := make([]grpc.CallOption, len(opts)+1)
	copy(callOpts, opts)
	callOpts[len(opts)] = grpc.Trailer(&trailer)

	err := invoker(ctx, method, req, reply, cc, callOpts...)

	elapsed := time.Since(startTime)

	var loggedResp any
	if err == nil {
		loggedResp = reply
	}
	var target string
	if cc != nil {
		target = cc.Target()
	}

	attrs := []any{
		slog.String("method", method),
		slog.String("target", target),
		slog.Any("req", req),
		slog.Any("resp", loggedResp),
		slog.Any("err", err),
		slog.String("code", status.Code(err).String()),
		slog.String("elapsed-time", elapsed.String()),
	}
	if vals := trailer.Get(ServerElapsedTrailer); len(vals) > 0 {
		attrs = append(attrs, slog.String("server-elapsed-us", vals[0]))
	}

	slog.InfoContext(ctx, "Call RPC", attrs...)

	return err
}
