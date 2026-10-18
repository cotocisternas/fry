package objectstore_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"testing"

	"github.com/cotocisternas/fry/internal/objectstore"
	"github.com/cotocisternas/fry/internal/testutil/s3fixture"
)

func adapter(t *testing.T, f *s3fixture.Fixture) *objectstore.S3 {
	t.Helper()
	s, e := objectstore.NewS3(context.Background(), f.Client(), f.Client(), "fry-test", "")
	if e != nil {
		t.Fatal(e)
	}
	return s
}

func TestS3ConditionalRequestsAndPermanentMarker(t *testing.T) {
	f := s3fixture.New(s3fixture.Config{})
	defer f.Close()
	s := adapter(t, f)
	ctx := context.Background()
	key := "packs/v1/ab/organization/attempt"
	body := []byte("pack bytes")
	digest := sha256.Sum256(body)
	if err := s.Create(ctx, key, bytes.NewReader(body), int64(len(body)), digest); err != nil {
		t.Fatal(err)
	}
	info, err := s.Stat(ctx, key)
	if err != nil || info.Size != int64(len(body)) || info.SHA256 != digest {
		t.Fatal(info, err)
	}
	requireOriginalRange(t, ctx, s, key, info.ETag)
	if err = s.Create(
		ctx,
		key,
		bytes.NewReader(body),
		int64(len(body)),
		digest,
	); !errors.Is(
		err,
		objectstore.ErrConflict,
	) {
		t.Fatal("overwrite allowed", err)
	}
	requirePermanentMarker(t, ctx, s, key, body, digest)
	requireConditionalRequests(t, f.Requests())
}

func requireConditionalRequests(t *testing.T, requests []s3fixture.Request) {
	t.Helper()
	var creates, replacements, heads int
	for _, q := range requests {
		switch q.Method {
		case "PUT":
			if requireConditionalPut(t, q) {
				creates++
			} else {
				replacements++
			}
		case "HEAD":
			heads++
			if q.ChecksumMode != "ENABLED" {
				t.Fatal("HEAD did not request verified checksum")
			}
		case "DELETE":
			t.Fatal("marker deleted")
		}
	}
	if creates < 2 || replacements != 1 || heads == 0 {
		t.Fatal("missing conditional operations", creates, replacements, heads)
	}
}

// requireConditionalPut reports whether the request is a create, rejecting any
// replacement that is not a zero-byte, checksum-bearing conditional marker.
func requireConditionalPut(t *testing.T, q s3fixture.Request) bool {
	t.Helper()
	if q.Checksum == "" {
		t.Fatal("unchecked upload")
	}
	if q.IfNoneMatch == "*" {
		return true
	}
	if q.IfMatch == "" || q.Length != 0 {
		t.Fatalf("unsafe PUT %+v", q)
	}
	return false
}

func requirePermanentMarker(
	t *testing.T,
	ctx context.Context,
	s *objectstore.S3,
	key string,
	body []byte,
	digest [32]byte,
) {
	t.Helper()
	if err := s.Retire(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := s.Retire(ctx, key); err != nil {
		t.Fatal("repeat retirement", err)
	}
	info, err := s.Stat(ctx, key)
	if err != nil || !info.Retired || info.Size != 0 || info.SHA256 != sha256.Sum256(nil) {
		t.Fatal("marker not verified", info, err)
	}
	if err := s.Create(
		ctx,
		key,
		bytes.NewReader(body),
		int64(len(body)),
		digest,
	); !errors.Is(
		err,
		objectstore.ErrConflict,
	) {
		t.Fatal("late uploader resurrected body", err)
	}
}

func requireOriginalRange(t *testing.T, ctx context.Context, s *objectstore.S3, key, etag string) {
	t.Helper()
	r, err := s.Range(ctx, key, etag, 2, 4)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	closeForTest(t, r)
	if err != nil || string(got) != "ck b" {
		t.Fatal(string(got), err)
	}
}

func TestS3RetirementBeforeDelayedCreate(t *testing.T) {
	f := s3fixture.New(s3fixture.Config{})
	defer f.Close()
	s := adapter(t, f)
	ctx := context.Background()
	key := "packs/v1/empty/attempt"
	if err := s.Retire(ctx, key); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("late"))
	if err := s.Create(ctx, key, bytes.NewReader([]byte("late")), 4, digest); !errors.Is(err, objectstore.ErrConflict) {
		t.Fatal(err)
	}
	object, _ := f.Object(key)
	if len(object.Body) != 0 || !object.Retired {
		t.Fatal("late data survived")
	}
}

func TestS3RejectsUnsafeBucketAndUnverifiedObject(t *testing.T) {
	for name, cfg := range map[string]s3fixture.Config{
		"versioned": {
			Versioning: "Enabled",
		},
		"suspended": {
			Versioning: "Suspended",
		},
		"lifecycle": {
			Lifecycle: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := s3fixture.New(cfg)
			defer f.Close()
			if _, err := objectstore.NewS3(context.Background(), f.Client(), f.Client(), "fry-test", ""); err == nil {
				t.Fatal("unsafe bucket accepted")
			}
		})
	}
	f := s3fixture.New(s3fixture.Config{MissingChecksum: true})
	defer f.Close()
	s := adapter(t, f)
	key := "packs/v1/no-checksum"
	digest := sha256.Sum256([]byte("body"))
	if err := s.Create(context.Background(), key, bytes.NewReader([]byte("body")), 4, digest); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stat(context.Background(), key); !errors.Is(err, objectstore.ErrIntegrity) {
		t.Fatal("unverified HEAD accepted", err)
	}
}

func closeForTest(t *testing.T, closer io.Closer) {
	t.Helper()
	if err := closer.Close(); err != nil {
		t.Error("close", err)
	}
}
