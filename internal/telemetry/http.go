package telemetry

import (
	"context"
	"io"
	"net/http"
	"sync"

	"github.com/felixge/httpsnoop"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// HTTPHandler records a fixed operation, bounded method, and response code.
// It preserves optional ResponseWriter interfaces and never records URLs,
// headers, bodies, query strings, or panic values. Public requests start a new
// trace so incoming sampled flags cannot override this process's sampling budget.
func (r *Runtime) HTTPHandler(operation string, next http.Handler) http.Handler {
	return r.httpHandler(operation, next, false)
}

// InternalHTTPHandler extracts trace context only on an already authenticated
// internal endpoint. Its remote-parent sampler still applies the local ratio.
func (r *Runtime) InternalHTTPHandler(operation string, next http.Handler) http.Handler {
	return r.httpHandler(operation, next, true)
}

func (r *Runtime) httpHandler(operation string, next http.Handler, internal bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx := WithRuntime(req.Context(), r)
		if r == nil || r.tracer == nil {
			next.ServeHTTP(w, req.WithContext(ctx))
			return
		}
		opts := []trace.SpanStartOption{trace.WithSpanKind(trace.SpanKindServer)}
		if internal {
			ctx = (propagation.TraceContext{}).Extract(ctx, propagation.HeaderCarrier(req.Header))
		} else {
			opts = append(opts, trace.WithNewRoot())
		}
		ctx, finish := r.start(ctx, operation,
			[]attribute.KeyValue{attribute.String("http.request.method", safeMethod(req.Method))},
			opts...)
		captured := httpsnoop.Metrics{Code: http.StatusOK}
		completed := false
		defer func() {
			finishHTTPHandler(ctx, captured.Code, completed, finish)
		}()
		captured.CaptureMetrics(w, func(wrapped http.ResponseWriter) {
			next.ServeHTTP(wrapped, req.WithContext(ctx))
		})
		completed = true
	})
}

func finishHTTPHandler(ctx context.Context, code int, completed bool, finish func(error)) {
	if !completed {
		code = http.StatusInternalServerError
	}
	code = safeStatusCode(code)
	trace.SpanFromContext(ctx).SetAttributes(attribute.Int("http.response.status_code", code))
	if !completed || code >= 500 {
		finish(errHTTP)
	} else {
		finish(nil)
	}
}

func safeStatusCode(code int) int {
	if code < 100 || code > 599 {
		return 0
	}
	return code
}

// HTTPTransport instruments a trusted internal HTTP client without changing its
// TLS, cancellation, or streaming behavior. The response body remains owned by
// the caller; EOF, Close, and errors end its span exactly once.
func (r *Runtime) HTTPTransport(operation string, base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	if r == nil || r.tracer == nil {
		return base
	}
	return transportFunc(func(req *http.Request) (*http.Response, error) {
		ctx, finish := r.start(req.Context(), operation,
			[]attribute.KeyValue{attribute.String("http.request.method", safeMethod(req.Method))},
			trace.WithSpanKind(trace.SpanKindClient))
		outbound := propagatedRequest(ctx, req)
		returned := false
		defer func() {
			if !returned {
				finish(errHTTP)
			}
		}()
		response, err := base.RoundTrip(outbound)
		returned = true
		if err != nil {
			finish(err)
			return response, err
		}
		if response == nil {
			finish(errHTTP)
			return nil, errHTTP
		}
		traceResponse(ctx, response, finish)
		return response, nil
	})
}

func propagatedRequest(ctx context.Context, req *http.Request) *http.Request {
	outbound := req.Clone(ctx)
	outbound.Header = req.Header.Clone()
	if outbound.Header == nil {
		outbound.Header = make(http.Header)
	}
	outbound.Header.Del("Baggage")
	outbound.Header.Del("Traceparent")
	outbound.Header.Del("Tracestate")
	(propagation.TraceContext{}).Inject(ctx, propagation.HeaderCarrier(outbound.Header))
	return outbound
}

func traceResponse(ctx context.Context, response *http.Response, finish func(error)) {
	code := safeStatusCode(response.StatusCode)
	trace.SpanFromContext(ctx).SetAttributes(attribute.Int("http.response.status_code", code))
	end := func(bodyErr error) {
		if bodyErr == nil && code >= 500 {
			bodyErr = errHTTP
		}
		finish(bodyErr)
	}
	if response.Body == nil || response.Body == http.NoBody {
		end(nil)
	} else {
		response.Body = &tracedBody{ReadCloser: response.Body, finish: end}
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type tracedBody struct {
	io.ReadCloser
	finish func(error)
	once   sync.Once
}

func (b *tracedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.once.Do(func() {
			if err == io.EOF {
				b.finish(nil)
			} else {
				b.finish(err)
			}
		})
	}
	return n, err
}

func (b *tracedBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(func() { b.finish(err) })
	return err
}

func safeMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodDelete, http.MethodConnect, http.MethodOptions, http.MethodTrace, http.MethodPatch:
		return method
	default:
		return "_OTHER"
	}
}
