//go:build linux

package admintransport

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/cotocisternas/fry/internal/admin"
)

func TestLegacySubmissionAdaptersRejectEachOthersFields(t *testing.T) {
	// No administration dependency is needed: malformed input must fail before
	// invoking a credential operation, even when the other endpoint accepts it.
	h := &HTTP{origin: &url.URL{Scheme: "https"}}
	for _, tc := range []struct {
		name string
		post legacyRoute
		body string
	}{
		{
			"password rejects access field", h.legacyPostSubmissionPassword,
			`{"organization":"example","username":"sender","password":"secret","enabled":true}`,
		},
		{
			"access rejects secret field", h.legacyPostSubmissionAccess,
			`{"organization":"example","username":"sender","enabled":false,"password":"secret"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/json")
			value, err := tc.post(httptest.NewRecorder(), r)
			if !errors.Is(err, admin.ErrInvalid) || value != nil {
				t.Fatalf("invalid payload returned %v, %v; want no value and ErrInvalid", value, err)
			}
		})
	}
}
