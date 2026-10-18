//go:build linux

package admintransport

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cotocisternas/fry/internal/admin"
	"github.com/cotocisternas/fry/internal/hotstore"

	"connectrpc.com/connect/v2"
)

// schemeHTTPS identifies HTTPS origins and selects secure cookies.
const schemeHTTPS = "https"

//go:embed web/*
var web embed.FS

type (
	HTTPOptions struct{ Origin string }
	HTTP        struct {
		service *admin.Service
		origin  *url.URL
		slots   chan struct{}
		oidc    *oidcAdapter
		rpc     http.Handler
	}
)

func NewHTTP(service *admin.Service, opts HTTPOptions) (*HTTP, error) {
	origin, err := url.Parse(opts.Origin)
	if err != nil || origin.Host == "" || origin.User != nil || origin.RawQuery != "" || origin.Fragment != "" ||
		origin.Path != "" {
		return nil, admin.ErrInvalid
	}
	if !validOriginScheme(origin) {
		return nil, admin.ErrInvalid
	}

	h := &HTTP{service: service, origin: origin, slots: make(chan struct{}, 64)}
	h.rpc = h.newRPC()
	return h, nil
}

func (h *HTTP) cookieName() string {
	if h.origin.Scheme == schemeHTTPS {
		return "__Host-fry-admin"
	}
	return "fry-admin-local"
}

func (h *HTTP) token(r *http.Request) string {
	cookie, err := r.Cookie(h.cookieName())
	if err != nil {
		return ""
	}
	return cookie.Value
}

func (h *HTTP) sessionCookie(session admin.Session) *http.Cookie {
	// Owner: admin maintainers; review by 2027-01-18.
	// #nosec G124 -- Secure follows NewHTTP's validated origin: HTTP is allowed only on literal loopback IPs.
	return &http.Cookie{
		Name:     h.cookieName(),
		Value:    session.Token,
		Path:     "/",
		Secure:   h.origin.Scheme == schemeHTTPS,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  session.Identity.Expires,
	}
}

func (h *HTTP) setSession(w http.ResponseWriter, session admin.Session) {
	http.SetCookie(w, h.sessionCookie(session))
}

func reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Warn("admin response encoding failed")
	}
}

func failure(w http.ResponseWriter, err error) {
	problem := rpcError(err)
	status := legacyErrorStatus(connect.CodeOf(problem))
	var rpcProblem *connect.Error
	message := "Administration is temporarily unavailable."
	if errors.As(problem, &rpcProblem) {
		message = rpcProblem.Message()
	}
	reply(w, status, map[string]string{"error": message})
}

func decode(w http.ResponseWriter, r *http.Request, value any) error {
	if strings.Split(r.Header.Get("Content-Type"), ";")[0] != "application/json" {
		return admin.ErrInvalid
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return admin.ErrInvalid
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return admin.ErrInvalid
	}
	return nil
}

func (h *HTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	securityHeaders(w)
	if r.Host != h.origin.Host {
		http.Error(w, "Unexpected host", 400)
		return
	}
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		http.Error(w, "Busy", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	r = r.WithContext(ctx)
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/fry.admin.v1.AdminService/") {
		h.rpc.ServeHTTP(w, r)
		return
	}
	if err := h.authorizeLegacy(r); err != nil {
		failure(w, err)
		return
	}
	if h.servePublic(w, r) {
		return
	}
	h.serveLegacy(w, r)
}

func securityHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().
		Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; object-src 'none'; "+
			"base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
}

func (h *HTTP) authorizeLegacy(r *http.Request) error {
	if r.Method != http.MethodPost {
		return nil
	}
	if r.Header.Get("Origin") != h.origin.String() {
		return admin.ErrForbidden
	}
	if r.URL.Path == "/api/login" {
		return nil
	}
	who, err := h.service.Identity(r.Context(), h.token(r))
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-FRY-CSRF")), []byte(who.CSRF)) != 1 {
		return admin.ErrForbidden
	}
	return nil
}

func (h *HTTP) servePublic(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	switch r.URL.Path {
	case "/", "/app.js", "/rpc.js", "/style.css":
		h.serveAsset(w, r)
	case "/api/config":
		reply(w, 200, map[string]bool{"oidc": h.oidc != nil})
	case "/auth/oidc", "/auth/callback":
		if h.oidc == nil {
			http.NotFound(w, r)
		} else {
			h.oidc.serve(h, w, r)
		}
	default:
		return false
	}
	return true
}

func (h *HTTP) serveAsset(w http.ResponseWriter, r *http.Request) {
	name, mime := strings.TrimPrefix(r.URL.Path, "/"), "text/javascript; charset=utf-8"
	if name == "" {
		name, mime = "index.html", "text/html; charset=utf-8"
	}
	if name == "style.css" {
		mime = "text/css; charset=utf-8"
	}
	data, err := web.ReadFile("web/" + name)
	if err != nil {
		failure(w, err)
		return
	}
	w.Header().Set("Content-Type", mime)
	// Owner: admin maintainers; review by 2027-01-18.
	// #nosec G705 -- Only the four fixed embedded asset names selected by servePublic reach this writer.
	if _, err := w.Write(data); err != nil {
		slog.WarnContext(r.Context(), "admin asset response failed")
	}
}

func (h *HTTP) serveContent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	token := h.token(r)
	var err error
	var content *admin.Content
	content, err = h.service.OpenContent(ctx, token, r.URL.Query().Get("organization"), r.URL.Query().Get("receipt"))
	if err == nil {
		defer func() {
			// Content implements io.Closer; its bounded audit cleanup must outlive cancellation.
			// io.Closer has no context parameter; Close bounds its independent audit cleanup.
			//nolint:contextcheck // io.Closer has no context parameter.
			if closeErr := content.Close(); closeErr != nil {
				// Storage/audit errors can contain private values or filesystem paths.
				slog.ErrorContext(ctx, "admin content close failed")
			}
		}()
		w.Header().Set("Content-Type", "message/rfc822")
		w.Header().Set("Content-Disposition", `attachment; filename="message.eml"`)
		w.Header().Set("Content-Length", strconv.FormatInt(content.Size, 10))
		if _, e := io.Copy(w, content); e != nil {
			panic(http.ErrAbortHandler)
		}
		return
	}
	if err != nil {
		failure(w, err)
	}
}

func identifier() (string, error) { id, err := hotstore.NewID(); return id.String(), err }

func memberStorageLimit(raw string) (int, error) {
	if raw == "" {
		return 64, nil
	}
	for _, c := range raw {
		if c < '0' || c > '9' {
			return 0, admin.ErrInvalid
		}
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > 64 {
		return 0, admin.ErrInvalid
	}
	return limit, nil
}

func validOriginScheme(origin *url.URL) bool {
	if origin.Scheme == schemeHTTPS {
		return true
	}
	ip := net.ParseIP(origin.Hostname())
	return origin.Scheme == "http" && ip != nil && ip.IsLoopback()
}

func legacyErrorStatus(code connect.Code) int {
	switch code {
	case connect.CodeUnauthenticated:
		return 401
	case connect.CodePermissionDenied:
		return 403
	case connect.CodeInvalidArgument:
		return 400
	case connect.CodeAlreadyExists:
		return 409
	case connect.CodeNotFound:
		return 404
	case connect.CodeCanceled,
		connect.CodeUnknown,
		connect.CodeDeadlineExceeded,
		connect.CodeResourceExhausted,
		connect.CodeFailedPrecondition,
		connect.CodeAborted,
		connect.CodeOutOfRange,
		connect.CodeUnimplemented,
		connect.CodeInternal,
		connect.CodeUnavailable,
		connect.CodeDataLoss:
		return http.StatusServiceUnavailable
	}
	return http.StatusServiceUnavailable
}
