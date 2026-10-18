package objectstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// S3 separates the producer/read client from the privileged retirement client.
// Deployment policy must deny producer overwrites/deletes and prevent deletion
// of empty markers. The adapter cannot prove IAM or bucket-policy correctness.
type S3 struct {
	producer, cleanup *s3.Client
	bucket            string
	namespace         string
	owner             *string
}

func NewS3(ctx context.Context, producer, cleanup *s3.Client, bucket, owner string) (*S3, error) {
	if producer == nil || cleanup == nil || bucket == "" {
		return nil, fmt.Errorf("objectstore: clients and bucket required")
	}
	// Detach endpoint pointers supplied by callers so later configuration edits
	// cannot redirect an adapter after its namespace has been captured.
	freezeClient := func(client *s3.Client) *s3.Client {
		opts := client.Options()
		if opts.BaseEndpoint != nil {
			opts.BaseEndpoint = aws.String(*opts.BaseEndpoint)
		}
		return s3.New(opts)
	}
	producer, cleanup = freezeClient(producer), freezeClient(cleanup)
	namespace, err := clientNamespace(producer, bucket, owner)
	if err != nil {
		return nil, err
	}
	cleanupNamespace, err := clientNamespace(cleanup, bucket, owner)
	if err != nil {
		return nil, err
	}
	if namespace != cleanupNamespace {
		return nil, fmt.Errorf("objectstore: producer and retirement namespaces differ")
	}
	store := &S3{producer: producer, cleanup: cleanup, bucket: bucket, namespace: namespace}
	if owner != "" {
		store.owner = &owner
	}
	if err := store.checkBucketPolicy(ctx); err != nil {
		return nil, err
	}
	return store, nil
}

func (store *S3) checkBucketPolicy(ctx context.Context) error {
	// Suspended versioning is unsafe too: overwriting would not erase old versions.
	version, err := store.cleanup.GetBucketVersioning(
		ctx,
		&s3.GetBucketVersioningInput{Bucket: &store.bucket, ExpectedBucketOwner: store.owner},
	)
	if err != nil {
		return err
	}
	if version.Status != "" {
		return fmt.Errorf("objectstore: pack bucket must never have been versioned")
	}
	lifecycle, err := store.cleanup.GetBucketLifecycleConfiguration(
		ctx,
		&s3.GetBucketLifecycleConfigurationInput{Bucket: &store.bucket, ExpectedBucketOwner: store.owner},
	)
	if err != nil && !apiCode(err, "NoSuchLifecycleConfiguration") {
		return err
	}
	if err == nil && len(lifecycle.Rules) > 0 {
		return fmt.Errorf("objectstore: lifecycle rules are not allowed on the permanent-marker bucket")
	}
	return nil
}

func apiCode(err error, codes ...string) bool {
	var api smithy.APIError
	if errors.As(err, &api) {
		for _, c := range codes {
			if api.ErrorCode() == c {
				return true
			}
		}
	}
	return false
}

func translate(err error) error {
	if apiCode(err, "NoSuchKey", "NotFound") {
		return fmt.Errorf("%w: %w", ErrMissing, err)
	}
	if apiCode(err, "PreconditionFailed", "ConditionalRequestConflict") {
		return fmt.Errorf("%w: %w", ErrConflict, err)
	}
	return err
}

func keyValid(key string) bool {
	return strings.HasPrefix(key, "packs/v1/") && len(key) > len("packs/v1/") && !strings.Contains(key, "..")
}

func (store *S3) Create(ctx context.Context, key string, body io.ReadSeeker, size int64, digest [32]byte) error {
	if !keyValid(key) || body == nil || size <= 0 || size > 256<<20 {
		return fmt.Errorf("objectstore: invalid create")
	}
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		return err
	}
	_, err := store.producer.PutObject(
		ctx,
		&s3.PutObjectInput{
			Bucket:              &store.bucket,
			Key:                 &key,
			Body:                body,
			ContentLength:       &size,
			IfNoneMatch:         aws.String("*"),
			ChecksumSHA256:      aws.String(base64.StdEncoding.EncodeToString(digest[:])),
			ContentType:         aws.String("application/octet-stream"),
			ExpectedBucketOwner: store.owner,
		},
	)
	return translate(err)
}

func (store *S3) Stat(ctx context.Context, key string) (Info, error) {
	return store.stat(ctx, store.producer, key)
}

func (store *S3) stat(ctx context.Context, client *s3.Client, key string) (Info, error) {
	var info Info
	if !keyValid(key) {
		return info, fmt.Errorf("objectstore: invalid key")
	}
	out, err := client.HeadObject(
		ctx,
		&s3.HeadObjectInput{
			Bucket:              &store.bucket,
			Key:                 &key,
			ChecksumMode:        types.ChecksumModeEnabled,
			ExpectedBucketOwner: store.owner,
		},
	)
	if err != nil {
		return info, translate(err)
	}
	return objectInfo(out)
}

func objectInfo(out *s3.HeadObjectOutput) (Info, error) {
	var info Info
	digest, err := base64.StdEncoding.DecodeString(aws.ToString(out.ChecksumSHA256))
	if err != nil || len(digest) != 32 || out.ContentLength == nil || out.ETag == nil || *out.ETag == "" {
		return info, ErrIntegrity
	}
	info.Size, info.ETag, info.Retired = *out.ContentLength, *out.ETag, out.Metadata["fry-retired"] == "true"
	copy(info.SHA256[:], digest)
	if info.Retired && (info.Size != 0 || info.SHA256 != sha256.Sum256(nil)) {
		return Info{}, ErrIntegrity
	}
	return info, nil
}

func (store *S3) Range(ctx context.Context, key, etag string, offset, length int64) (io.ReadCloser, error) {
	if !keyValid(key) || etag == "" || offset < 0 || length <= 0 || offset > 1<<62 || length > 256<<20 {
		return nil, fmt.Errorf("objectstore: invalid range")
	}
	out, err := store.producer.GetObject(
		ctx,
		&s3.GetObjectInput{
			Bucket:              &store.bucket,
			Key:                 &key,
			IfMatch:             &etag,
			Range:               aws.String(fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)),
			ExpectedBucketOwner: store.owner,
		},
	)
	if err != nil {
		return nil, translate(err)
	}
	if err := validateRangeResponse(out, etag, offset, length); err != nil {
		return nil, errors.Join(err, out.Body.Close())
	}
	return out.Body, nil
}

func validateRangeResponse(out *s3.GetObjectOutput, etag string, offset, length int64) error {
	var start, end, total int64
	n, e := fmt.Sscanf(aws.ToString(out.ContentRange), "bytes %d-%d/%d", &start, &end, &total)
	if e != nil || n != 3 || start != offset || end != offset+length-1 || total <= end ||
		aws.ToInt64(out.ContentLength) != length ||
		aws.ToString(out.ETag) != etag {
		return ErrIntegrity
	}
	return nil
}

func (store *S3) Retire(ctx context.Context, key string) error {
	if !keyValid(key) {
		return fmt.Errorf("objectstore: invalid retirement key")
	}
	for range 8 {
		if err := ctx.Err(); err != nil {
			return err
		}
		done, err := store.retireAttempt(ctx, key)
		if err != nil || done {
			return err
		}
	}
	return ErrConflict
}

func (store *S3) retireAttempt(ctx context.Context, key string) (bool, error) {
	info, err := store.stat(ctx, store.cleanup, key)
	if err == nil && info.Retired {
		return true, nil
	}
	if err != nil && !errors.Is(err, ErrMissing) {
		return false, err
	}
	return store.replaceWithMarker(ctx, key, info, errors.Is(err, ErrMissing))
}

func (store *S3) replaceWithMarker(ctx context.Context, key string, info Info, missing bool) (bool, error) {
	digest := sha256.Sum256(nil)
	input := &s3.PutObjectInput{
		Bucket:              &store.bucket,
		Key:                 &key,
		Body:                bytes.NewReader(nil),
		ContentLength:       aws.Int64(0),
		ChecksumSHA256:      aws.String(base64.StdEncoding.EncodeToString(digest[:])),
		Metadata:            map[string]string{"fry-retired": "true"},
		ExpectedBucketOwner: store.owner,
	}
	if missing {
		input.IfNoneMatch = aws.String("*")
	} else {
		input.IfMatch = &info.ETag
	}
	_, putErr := store.cleanup.PutObject(ctx, input)
	// Resolve success and lost replies by reading the actual marker. A conflict
	// means a delayed creator may have won first; re-read and replace its body.
	after, headErr := store.stat(ctx, store.cleanup, key)
	if headErr == nil && after.Retired {
		return true, nil
	}
	if putErr != nil && !apiCode(putErr, "PreconditionFailed", "ConditionalRequestConflict") {
		return false, errors.Join(putErr, headErr)
	}
	if headErr != nil && !errors.Is(headErr, ErrMissing) {
		return false, headErr
	}
	return false, nil
}
