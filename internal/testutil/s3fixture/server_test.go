package s3fixture

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

type failedResponse struct{ header http.Header }

func (w *failedResponse) Header() http.Header       { return w.header }
func (w *failedResponse) WriteHeader(int)           {}
func (w *failedResponse) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestResponseWriteFailureAbortsHTTP(t *testing.T) {
	for _, target := range []string{
		"/fry-test/missing",
		"/?versioning",
		"/?lifecycle",
		"/fry-test/key",
	} {
		t.Run(target, func(t *testing.T) {
			fixture := &Fixture{
				config:  Config{Lifecycle: true},
				objects: map[string]Object{"key": {Body: []byte("body")}},
			}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://fixture"+target, nil)
			req.Header.Set("Range", "bytes=0-3")
			defer func() {
				recovered := recover()
				err, ok := recovered.(error)
				if !ok || !errors.Is(err, http.ErrAbortHandler) {
					t.Fatalf("expected HTTP abort, got %v", recovered)
				}
			}()
			fixture.serve(&failedResponse{header: make(http.Header)}, req)
		})
	}
}
