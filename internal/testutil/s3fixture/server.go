// Package s3fixture provides a deliberately narrow, in-memory HTTP contract
// fixture. It is NOT an S3 emulator or proof of AWS consistency/IAM behavior.
package s3fixture

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type Object struct {
	Body    []byte
	Digest  [32]byte
	ETag    string
	Retired bool
}
type Request struct {
	Method, Key, IfMatch, IfNoneMatch, Range, ChecksumMode, Checksum string
	Length                                                           int64
}
type Config struct {
	Versioning      string `json:"versioning"`
	Lifecycle       bool   `json:"lifecycle"`
	MissingChecksum bool   `json:"missing_checksum"`
}
type Fixture struct {
	unavailable atomic.Bool
	Server      *httptest.Server
	mu          sync.Mutex
	objects     map[string]Object
	requests    []Request
	serial      int
	config      Config
	closed      bool
}

func New(config Config) *Fixture {
	f := &Fixture{objects: map[string]Object{}, config: config}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}
func (f *Fixture) SetUnavailable(unavailable bool) { f.unavailable.Store(unavailable) }
func (f *Fixture) Close()                          { f.Server.Close(); f.mu.Lock(); f.closed = true; f.mu.Unlock() }

func (f *Fixture) Client() *s3.Client {
	return s3.New(
		s3.Options{
			Region:                     "sa-east-1",
			BaseEndpoint:               aws.String(f.Server.URL),
			UsePathStyle:               true,
			Credentials:                aws.AnonymousCredentials{},
			RetryMaxAttempts:           1,
			RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
			ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		},
	)
}

func (f *Fixture) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.requests...)
}

func (f *Fixture) Object(key string) (Object, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[key]
	o.Body = append([]byte(nil), o.Body...)
	return o, ok
}

func respondError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	if _, err := fmt.Fprintf(w, "<Error><Code>%s</Code><Message>fixture failure</Message></Error>", code); err != nil {
		panic(http.ErrAbortHandler)
	}
}

func (f *Fixture) serve(w http.ResponseWriter, r *http.Request) {
	if f.unavailable.Load() {
		respondError(w, http.StatusServiceUnavailable, "ServiceUnavailable")
		return
	}
	if r.URL.Query().Has("versioning") {
		w.Header().Set("Content-Type", "application/xml")
		if _, err := fmt.Fprintf(
			w,
			"<VersioningConfiguration><Status>%s</Status></VersioningConfiguration>",
			f.config.Versioning,
		); err != nil {
			panic(http.ErrAbortHandler)
		}
		return
	}
	if r.URL.Query().Has("lifecycle") {
		if !f.config.Lifecycle {
			respondError(w, 404, "NoSuchLifecycleConfiguration")
			return
		}
		if _, err := io.WriteString(
			w,
			"<LifecycleConfiguration><Rule><ID>expiry</ID><Status>Enabled</Status><Filter><Prefix"+
				"></Prefix></Filter><Expiration><Days>1</Days></Expiration></Rule></LifecycleConfigur"+
				"ation>",
		); err != nil {
			panic(http.ErrAbortHandler)
		}
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/fry-test/")
	f.mu.Lock()
	f.requests = append(
		f.requests,
		Request{
			Method:       r.Method,
			Key:          key,
			IfMatch:      r.Header.Get("If-Match"),
			IfNoneMatch:  r.Header.Get("If-None-Match"),
			Range:        r.Header.Get("Range"),
			ChecksumMode: r.Header.Get("X-Amz-Checksum-Mode"),
			Checksum:     r.Header.Get("X-Amz-Checksum-Sha256"),
			Length:       r.ContentLength,
		},
	)
	f.mu.Unlock()
	if r.Method == http.MethodPut {
		f.put(w, r, key)
		return
	}
	f.getOrHead(w, r, key)
}

func (f *Fixture) put(w http.ResponseWriter, r *http.Request, key string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		respondError(w, 400, "InvalidRequest")
		return
	}
	digest := sha256.Sum256(body)
	if r.Header.Get("X-Amz-Checksum-Sha256") != base64.StdEncoding.EncodeToString(digest[:]) {
		respondError(w, 400, "BadDigest")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.allowPut(w, r, key) {
		return
	}
	if f.serial == int(^uint(0)>>1) {
		respondError(w, 500, "InternalError")
		return
	}
	f.serial++
	etag := fmt.Sprintf("\"opaque-%d\"", f.serial)
	f.objects[key] = Object{
		Body:    body,
		Digest:  digest,
		ETag:    etag,
		Retired: r.Header.Get("X-Amz-Meta-Fry-Retired") == "true",
	}
	w.Header().Set("ETag", etag)
	w.Header().Set("X-Amz-Checksum-Sha256", base64.StdEncoding.EncodeToString(digest[:]))
	w.WriteHeader(200)
}

func (f *Fixture) getOrHead(w http.ResponseWriter, r *http.Request, key string) {
	f.mu.Lock()
	obj, exists := f.objects[key]
	f.mu.Unlock()
	if !exists {
		respondError(w, 404, "NoSuchKey")
		return
	}
	if match := r.Header.Get("If-Match"); match != "" && match != obj.ETag {
		respondError(w, 412, "PreconditionFailed")
		return
	}
	w.Header().Set("ETag", obj.ETag)
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", strconv.Itoa(len(obj.Body)))
		if !f.config.MissingChecksum {
			w.Header().Set("X-Amz-Checksum-Sha256", base64.StdEncoding.EncodeToString(obj.Digest[:]))
		}
		if obj.Retired {
			w.Header().Set("X-Amz-Meta-Fry-Retired", "true")
		}
		return
	}
	if r.Method == http.MethodGet {
		writeObjectRange(w, r, obj)
		return
	}
	respondError(w, 405, "MethodNotAllowed")
}

// Caller holds mu for both conditional authorization and publication.
func (f *Fixture) allowPut(w http.ResponseWriter, r *http.Request, key string) bool {
	old, exists := f.objects[key]
	if r.Header.Get("If-None-Match") == "*" {
		if exists {
			respondError(w, 412, "PreconditionFailed")
			return false
		}
	} else if match := r.Header.Get("If-Match"); match != "" {
		if !exists || old.ETag != match {
			respondError(w, 412, "PreconditionFailed")
			return false
		}
	} else {
		respondError(w, 403, "AccessDenied")
		return false
	}
	return true
}

func writeObjectRange(w http.ResponseWriter, r *http.Request, obj Object) {
	var start, end int64
	if _, err := fmt.Sscanf(
		r.Header.Get("Range"),
		"bytes=%d-%d",
		&start,
		&end,
	); err != nil || start < 0 || end < start ||
		end >= int64(len(obj.Body)) {
		respondError(w, 416, "InvalidRange")
		return
	}
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(obj.Body)))
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.WriteHeader(206)
	if _, err := w.Write(obj.Body[start : end+1]); err != nil {
		panic(http.ErrAbortHandler)
	}
}
