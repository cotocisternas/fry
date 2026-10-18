package objectstore

// This test is an explicit real-AWS contract probe. It never provisions or cleans
// up resources. Its durable manifest is the provisioning identity's cleanup input.
import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
)

const validationDuration = 5 * time.Minute

var validationBucketName = regexp.MustCompile(`^fry-validation-[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

type validationManifest struct {
	Version       int      `json:"version"`
	Bucket        string   `json:"bucket"`
	ExpectedOwner string   `json:"expected_bucket_owner"`
	Region        string   `json:"region"`
	Namespace     string   `json:"namespace"`
	ProducerARN   string   `json:"producer_arn"`
	RetirementARN string   `json:"retirement_arn"`
	RunID         string   `json:"run_id"`
	Keys          []string `json:"keys"`
}

func validationOptIn(configPath, bucketAck, manifestPath string) (bool, error) {
	if configPath == "" && bucketAck == "" && manifestPath == "" {
		return false, nil
	}
	if configPath == "" || bucketAck == "" || manifestPath == "" || !filepath.IsAbs(manifestPath) {
		return false, fmt.Errorf(
			"AWS validation requires config, exact bucket acknowledgment, and absolute new manifest path",
		)
	}
	return true, nil
}

// readValidationConfig rejects special files before opening. The operator owns
// the local path; this is not a defense against a hostile filesystem rename race.
func readValidationConfig(path string) (S3Config, error) {
	var c S3Config
	// Opted-in operator-selected config path; Lstat checks regular file and bounded size before open; owner: FRY
	// maintainers; expires: 2027-01-18.
	// #nosec G703 -- See the path-safety rationale above.
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 16<<10 {
		return c, fmt.Errorf("AWS validation config must be a regular file within 16KiB")
	}
	// Operator owns config path; preceding Lstat requires bounded regular file; hostile rename races are outside
	// this fixture contract; owner: FRY maintainers; expires: 2027-01-18.
	// #nosec G304 G703 -- See the path-safety rationale above.
	file, err := os.Open(path)
	if err != nil {
		return c, fmt.Errorf("AWS validation config cannot be opened")
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, (16<<10)+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(raw) > 16<<10 {
		return c, fmt.Errorf("AWS validation config is unreadable or exceeds its bound")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&c); err != nil {
		return S3Config{}, fmt.Errorf("AWS validation config is malformed")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return S3Config{}, fmt.Errorf("AWS validation config has trailing data")
	}
	return c, nil
}

func validateLiveConfig(c S3Config, ack string) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if !validationDestination(c, ack) || c.ExpectedOwner == "" || strings.TrimSpace(c.ProducerProfile) == "" ||
		strings.TrimSpace(c.RetirementProfile) == "" ||
		c.ProducerProfile == c.RetirementProfile {
		return fmt.Errorf("AWS validation configuration does not identify the isolated reviewed bucket/roles")
	}
	return nil
}

func validationDestination(c S3Config, ack string) bool {
	return c.Region == "sa-east-1" && c.Endpoint == "" && !c.LoopbackAnonymous && !c.UsePathStyle &&
		validationBucketName.MatchString(c.Bucket) &&
		len(c.Bucket) <= 63 &&
		c.Bucket == ack
}

func validateValidationPrincipal(
	account string,
	creds aws.Credentials,
	identity *sts.GetCallerIdentityOutput,
	now time.Time,
) (string, error) {
	if err := validateTemporarySession(creds, now); err != nil {
		return "", err
	}
	if !accountID.MatchString(account) || identity == nil || aws.ToString(identity.Account) != account {
		return "", fmt.Errorf("AWS validation principal account mismatch")
	}
	roleSession, err := validationRoleSession(account, aws.ToString(identity.Arn))
	if err != nil {
		return "", err
	}
	return validationStableRoleID(identity, roleSession[len(roleSession)-1])
}

func validationStableRoleID(identity *sts.GetCallerIdentityOutput, expectedSession string) (string, error) {
	roleID, session, ok := strings.Cut(aws.ToString(identity.UserId), ":")
	if !ok || !strings.HasPrefix(roleID, "AROA") || len(roleID) <= 4 || session == "" || session != expectedSession {
		return "", fmt.Errorf("AWS validation requires a stable assumed-role ID")
	}
	return roleID, nil
}

func validateTemporarySession(creds aws.Credentials, now time.Time) error {
	if creds.AccessKeyID == "" || creds.SecretAccessKey == "" || creds.SessionToken == "" {
		return fmt.Errorf("AWS validation requires temporary session credentials")
	}
	if creds.CanExpire && !creds.Expires.After(now.Add(validationDuration+time.Minute)) {
		return fmt.Errorf("AWS validation credentials expire before the bounded run and safety margin")
	}
	return nil
}

func validationRoleSession(account, raw string) ([]string, error) {
	arn := strings.SplitN(raw, ":", 6)
	if len(arn) != 6 || strings.Join(arn[:5], ":") != "arn:aws:sts::"+account ||
		!strings.HasPrefix(arn[5], "assumed-role/") {
		return nil, fmt.Errorf("AWS validation requires an assumed role in the expected account")
	}
	roleSession := strings.Split(strings.TrimPrefix(arn[5], "assumed-role/"), "/")
	if len(roleSession) < 2 || roleSession[len(roleSession)-1] == "" {
		return nil, fmt.Errorf("AWS validation malformed assumed role")
	}
	if slices.Contains(roleSession, "") {
		return nil, fmt.Errorf("AWS validation malformed assumed role")
	}
	return roleSession, nil
}

func requireAccessDenied(err error) error {
	var api smithy.APIError
	var status interface{ HTTPStatusCode() int }
	if err == nil || !errors.As(err, &api) || api.ErrorCode() != "AccessDenied" || !errors.As(err, &status) ||
		status.HTTPStatusCode() != 403 {
		return fmt.Errorf("expected HTTP 403 AccessDenied, got %s", validationErrorLabel(err))
	}
	return nil
}

func validationErrorLabel(err error) string {
	if err == nil {
		return "success"
	}
	var status interface{ HTTPStatusCode() int }
	code := "non-API-error"
	httpStatus := 0
	if api, ok := errors.AsType[smithy.APIError](err); ok {
		code = api.ErrorCode()
	}
	if errors.As(err, &status) {
		httpStatus = status.HTTPStatusCode()
	}
	return fmt.Sprintf("%s HTTP%d", code, httpStatus)
}

func writeValidationManifest(path string, m validationManifest) (result error) {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("AWS validation manifest path must be absolute")
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	// Opted-in operator-selected absolute manifest path; O_EXCL and 0600 prevent replacing existing authority;
	// owner: FRY maintainers; expires: 2027-01-18.
	// #nosec G304 G703 -- See the path-safety rationale above.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	// A failed/partial manifest remains evidence; the caller must not write S3.
	if _, err = f.Write(raw); err != nil {
		result = err
	} else {
		result = f.Sync()
	}
	result = errors.Join(result, f.Close())
	if result != nil {
		return result
	}
	// Syncs the parent of the validated absolute manifest path to persist its exclusive creation; owner: FRY
	// maintainers; expires: 2027-01-18.
	// #nosec G703 -- See the path-safety rationale above.
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

func TestAWSS3Validation(t *testing.T) {
	c, manifestPath := liveValidationConfig(t)
	v := newAWSValidationCase(t, c, manifestPath)
	v.checkOriginal()
	v.checkProducerAndRetirementWrites()
	v.checkPermanentMarkers()
	v.checkCreateRetireRace()
	v.checkFinalBucket()
}

func liveValidationConfig(t *testing.T) (S3Config, string) {
	t.Helper()
	configPath := os.Getenv("FRY_AWS_S3_VALIDATION_CONFIG")
	ack := os.Getenv("FRY_AWS_S3_VALIDATION_BUCKET")
	manifestPath := os.Getenv("FRY_AWS_S3_VALIDATION_MANIFEST")
	enabled, err := validationOptIn(configPath, ack, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !enabled {
		t.Skip("real AWS validation is not opted in")
	}
	// Refuse overwrite before loading credentials or building network clients.
	// Opt-in requires operator-selected absolute manifest path; Lstat rejects pre-existing output before AWS
	// operations; owner: FRY maintainers; expires: 2027-01-18.
	// #nosec G703 -- See the path-safety rationale above.
	if _, err := os.Lstat(manifestPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("AWS validation manifest must not already exist")
	}
	c, err := readValidationConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateLiveConfig(c, ack); err != nil {
		t.Fatal(err)
	}
	return c, manifestPath
}

type awsValidationCase struct {
	t                                                             *testing.T
	ctx                                                           context.Context
	c                                                             S3Config
	store                                                         *S3
	producer, retirement                                          *s3.Client
	imageKey, missingKey, raceKey, producerFresh, retirementFresh string
	body                                                          []byte
	digest                                                        [32]byte
	info                                                          Info
	runID                                                         string
	manifest                                                      validationManifest
}

func newAWSValidationCase(t *testing.T, c S3Config, manifestPath string) *awsValidationCase {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), validationDuration)
	t.Cleanup(cancel)
	// Ignore SDK-configured service endpoints, including nested AssumeRole/SSO
	// providers. Profiles, credential_process, proxies, and host trust remain trusted.
	t.Setenv("AWS_IGNORE_CONFIGURED_ENDPOINT_URLS", "true")
	producer, retirement, err := c.clients(ctx)
	if err != nil {
		t.Fatal("AWS validation client configuration failed")
	}
	producer, producerIdentity, producerRole := freezeValidationClient(t, ctx, c, producer)
	retirement, retirementIdentity, retirementRole := freezeValidationClient(t, ctx, c, retirement)
	if producerRole == retirementRole {
		t.Fatal("AWS validation profiles resolve to the same role")
	}
	checkValidationBucket(t, ctx, c, retirement)
	store, err := NewS3(ctx, producer, retirement, c.Bucket, c.ExpectedOwner)
	if err != nil {
		t.Fatalf("AWS validation bucket contract failed: %s", validationErrorLabel(err))
	}
	var random [16]byte
	if _, err = rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	runID := hex.EncodeToString(random[:])
	prefix := "packs/v1/validation/" + runID + "/"
	imageKey, missingKey, raceKey := prefix+"image", prefix+"missing-marker", prefix+"race"
	producerFresh, retirementFresh := prefix+"producer-forbidden", prefix+"retirement-forbidden"
	manifest := validationManifest{
		Version:       1,
		Bucket:        c.Bucket,
		ExpectedOwner: c.ExpectedOwner,
		Region:        c.Region,
		Namespace:     store.Namespace(),
		ProducerARN:   aws.ToString(producerIdentity.Arn),
		RetirementARN: aws.ToString(retirementIdentity.Arn),
		RunID:         runID,
		Keys:          []string{imageKey, missingKey, raceKey, producerFresh, retirementFresh},
	}
	if err = writeValidationManifest(manifestPath, manifest); err != nil {
		t.Fatal("AWS validation manifest publication failed; no writes attempted")
	}
	t.Logf("AWS validation manifest committed: run=%s keys=%d", runID, len(manifest.Keys))
	body := bytes.Repeat([]byte("FRY synthetic AWS contract\r\n"), 127)
	digest := sha256.Sum256(body)
	return &awsValidationCase{
		t:               t,
		ctx:             ctx,
		c:               c,
		store:           store,
		producer:        producer,
		retirement:      retirement,
		imageKey:        imageKey,
		missingKey:      missingKey,
		raceKey:         raceKey,
		producerFresh:   producerFresh,
		retirementFresh: retirementFresh,
		body:            body,
		digest:          digest,
		runID:           runID,
		manifest:        manifest,
	}
}

func (v *awsValidationCase) create(key string) error {
	return v.store.Create(v.ctx, key, bytes.NewReader(v.body), int64(len(v.body)), v.digest)
}

func (v *awsValidationCase) checkOriginal() {
	v.t.Helper()
	if err := v.create(v.imageKey); err != nil {
		v.t.Fatalf("AWS create failed: %s", validationErrorLabel(err))
	}
	var err error
	v.info, err = v.store.Stat(v.ctx, v.imageKey)
	if err != nil || v.info.Size != int64(len(v.body)) || v.info.SHA256 != v.digest || v.info.Retired ||
		v.info.ETag == "" {
		v.t.Fatalf("AWS HEAD integrity failed: %s", validationErrorLabel(err))
	}
	v.checkRangesAndDuplicateCreate()
}

func (v *awsValidationCase) checkRangesAndDuplicateCreate() {
	v.t.Helper()
	reader, err := v.store.Range(v.ctx, v.imageKey, v.info.ETag, 7, 53)
	if err != nil {
		v.t.Fatalf("AWS range failed: %s", validationErrorLabel(err))
	}
	got, readErr := io.ReadAll(io.LimitReader(reader, 54))
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(got, v.body[7:60]) {
		v.t.Fatal("AWS range bytes or closure invalid")
	}
	if err := v.create(v.imageKey); !errors.Is(err, ErrConflict) {
		v.t.Fatalf("AWS duplicate create not conflict: %s", validationErrorLabel(err))
	}
	stale, err := v.store.Range(v.ctx, v.imageKey, `"fry-validation-stale-etag"`, 0, 1)
	if stale != nil {
		if closeErr := stale.Close(); closeErr != nil {
			v.t.Error("stale range close", closeErr)
		}
	}
	if !errors.Is(err, ErrConflict) {
		v.t.Fatalf("AWS stale ETag not conflict: %s", validationErrorLabel(err))
	}
}

func (v *awsValidationCase) assertImage() {
	v.t.Helper()
	current, e := v.store.Stat(v.ctx, v.imageKey)
	if e != nil || current != v.info {
		v.t.Fatal("AWS image changed during forbidden operation")
	}
}

func (v *awsValidationCase) rawPut(client *s3.Client, key string, match *string) error {
	_, e := client.PutObject(
		v.ctx,
		&s3.PutObjectInput{
			Bucket:              &v.c.Bucket,
			Key:                 &key,
			ExpectedBucketOwner: &v.c.ExpectedOwner,
			Body:                bytes.NewReader(v.body),
			ContentLength:       aws.Int64(int64(len(v.body))),
			IfMatch:             match,
		},
	)
	return e
}

func (v *awsValidationCase) checkProducerAndRetirementWrites() {
	v.t.Helper()
	// Successful reads bracket policy probes using the same frozen sessions.
	v.assertImage()
	for _, key := range []string{v.imageKey, v.producerFresh} {
		validationDenied(v.t, "producer unconditional PUT", func() error { return v.rawPut(v.producer, key, nil) })
		v.assertImage()
	}
	validationDenied(
		v.t,
		"producer valid If-Match overwrite",
		func() error { return v.rawPut(v.producer, v.imageKey, &v.info.ETag) },
	)
	v.assertImage()
	retirementInfo, e := v.store.stat(v.ctx, v.retirement, v.imageKey)
	if e != nil || retirementInfo != v.info {
		v.t.Fatal("retirement positive HEAD failed")
	}
	for _, key := range []string{v.imageKey, v.retirementFresh} {
		validationDenied(v.t, "retirement unconditional PUT", func() error { return v.rawPut(v.retirement, key, nil) })
		current, e := v.store.stat(v.ctx, v.retirement, v.imageKey)
		if e != nil || current != v.info {
			v.t.Fatal("retirement image changed or positive HEAD failed")
		}
	}
	for _, key := range []string{v.producerFresh, v.retirementFresh} {
		if _, e := v.store.Stat(v.ctx, key); !errors.Is(e, ErrMissing) {
			v.t.Fatal("forbidden fresh PUT left an object or absence is unverified")
		}
	}
}

func (v *awsValidationCase) checkPermanentMarkers() {
	v.t.Helper()
	for _, key := range []string{v.imageKey, v.missingKey} {
		if err := v.store.Retire(v.ctx, key); err != nil {
			v.t.Fatalf("AWS retirement failed: %s", validationErrorLabel(err))
		}
		if err := v.store.Retire(v.ctx, key); err != nil {
			v.t.Fatal("AWS idempotent retirement failed")
		}
	}
	marker := assertValidationMarker(v.t, v.ctx, v.store, v.imageKey)
	assertValidationMarker(v.t, v.ctx, v.store, v.missingKey)
	for _, client := range []*s3.Client{v.producer, v.retirement} {
		v.checkMarkerDeletionDenied(client, marker)
	}
}

func (v *awsValidationCase) checkMarkerDeletionDenied(client *s3.Client, marker Info) {
	v.t.Helper()
	positive, e := v.store.stat(v.ctx, client, v.imageKey)
	if e != nil || positive != marker {
		v.t.Fatal("positive marker HEAD failed before delete probes")
	}
	for _, condition := range []*string{nil, &marker.ETag, aws.String("*")} {
		validationDenied(v.t, "marker DELETE", func() error {
			_, e := client.DeleteObject(
				v.ctx,
				&s3.DeleteObjectInput{
					Bucket:              &v.c.Bucket,
					Key:                 &v.imageKey,
					ExpectedBucketOwner: &v.c.ExpectedOwner,
					IfMatch:             condition,
				},
			)
			return e
		})
		if assertValidationMarker(v.t, v.ctx, v.store, v.imageKey) != marker {
			v.t.Fatal("forbidden DELETE changed marker")
		}
	}
	positive, e = v.store.stat(v.ctx, client, v.imageKey)
	if e != nil || positive != marker {
		v.t.Fatal("positive marker HEAD failed after delete probes")
	}
}

func (v *awsValidationCase) checkCreateRetireRace() {
	v.t.Helper()
	// Joined operations cannot outlive the test/run or race the final observation.
	var wg sync.WaitGroup
	wg.Add(2)
	var createErr, retireErr error
	go func() { defer wg.Done(); createErr = v.create(v.raceKey) }()
	go func() { defer wg.Done(); retireErr = v.store.Retire(v.ctx, v.raceKey) }()
	wg.Wait()
	if createErr != nil && !errors.Is(createErr, ErrConflict) {
		v.t.Fatalf("AWS raced create failed unexpectedly: %s", validationErrorLabel(createErr))
	}
	if retireErr != nil {
		v.t.Fatalf("AWS raced retirement failed: %s", validationErrorLabel(retireErr))
	}
	assertValidationMarker(v.t, v.ctx, v.store, v.raceKey)
	for _, key := range []string{v.imageKey, v.missingKey, v.raceKey} {
		if err := v.create(key); !errors.Is(err, ErrConflict) {
			v.t.Fatalf("AWS late producer not conflict: %s", validationErrorLabel(err))
		}
		assertValidationMarker(v.t, v.ctx, v.store, key)
	}
}

func (v *awsValidationCase) checkFinalBucket() {
	v.t.Helper()
	checkValidationBucket(v.t, v.ctx, v.c, v.retirement)
	if _, err := NewS3(v.ctx, v.producer, v.retirement, v.c.Bucket, v.c.ExpectedOwner); err != nil {
		v.t.Fatalf("AWS bucket contract drifted: %s", validationErrorLabel(err))
	}
	v.t.Logf(
		"AWS S3 contract verified: run=%s keys=%d markers=3; objects retained "+
			"for separate provisioning cleanup; retirement role remains trusted",
		v.runID,
		len(v.manifest.Keys),
	)
}

func freezeValidationClient(
	t *testing.T,
	ctx context.Context,
	c S3Config,
	client *s3.Client,
) (*s3.Client, *sts.GetCallerIdentityOutput, string) {
	t.Helper()
	opts := client.Options()
	creds, e := opts.Credentials.Retrieve(ctx)
	if e != nil {
		t.Fatal("AWS validation credential retrieval failed")
	}
	if creds.AccessKeyID == "" || creds.SecretAccessKey == "" || creds.SessionToken == "" ||
		(creds.CanExpire && !creds.Expires.After(time.Now().Add(validationDuration+time.Minute))) {
		t.Fatal("AWS validation requires temporary credentials valid beyond the bounded run")
	}
	frozen := aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) { return creds, nil })
	// Construct STS from explicit values, not LoadDefaultConfig's ambient endpoint
	// settings. Identity and S3 requests use this exact immutable credential value.
	identityClient := sts.NewFromConfig(
		aws.Config{Region: c.Region, Credentials: frozen, HTTPClient: opts.HTTPClient, RetryMaxAttempts: 1},
	)
	identity, e := identityClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if e != nil {
		t.Fatalf("AWS validation identity failed: %s", validationErrorLabel(e))
	}
	roleID, e := validateValidationPrincipal(c.ExpectedOwner, creds, identity, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	opts.Credentials = frozen
	return s3.New(opts), identity, roleID
}

func checkValidationBucket(t *testing.T, ctx context.Context, c S3Config, retirement *s3.Client) {
	t.Helper()
	location, e := retirement.GetBucketLocation(
		ctx,
		&s3.GetBucketLocationInput{Bucket: &c.Bucket, ExpectedBucketOwner: &c.ExpectedOwner},
	)
	if e != nil || string(location.LocationConstraint) != c.Region {
		t.Fatalf("AWS validation bucket location mismatch: %s", validationErrorLabel(e))
	}
	tags, e := retirement.GetBucketTagging(
		ctx,
		&s3.GetBucketTaggingInput{Bucket: &c.Bucket, ExpectedBucketOwner: &c.ExpectedOwner},
	)
	if e != nil {
		t.Fatalf("AWS validation bucket tags unavailable: %s", validationErrorLabel(e))
	}
	purpose := false
	for _, tag := range tags.TagSet {
		if aws.ToString(tag.Key) == "purpose" && aws.ToString(tag.Value) == "fry-validation" {
			purpose = true
		}
	}
	if !purpose {
		t.Fatal("AWS validation bucket purpose tag missing")
	}
}

func assertValidationMarker(t *testing.T, ctx context.Context, store *S3, key string) Info {
	t.Helper()
	marker, e := store.Stat(ctx, key)
	if e != nil || !marker.Retired || marker.Size != 0 || marker.SHA256 != sha256.Sum256(nil) || marker.ETag == "" {
		t.Fatal("AWS marker integrity failed")
	}
	return marker
}

func validationDenied(t *testing.T, name string, probe func() error) {
	t.Helper()
	if e := requireAccessDenied(probe()); e != nil {
		t.Fatalf("%s: %v", name, e)
	}
	t.Logf("negative %s: AccessDenied HTTP403", name)
}
