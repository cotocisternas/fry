package objectstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cotocisternas/fry/internal/testutil/s3fixture"

	"github.com/aws/aws-sdk-go-v2/aws"
)

func TestS3ConfigValidation(t *testing.T) {
	base := S3Config{
		Region:            "sa-east-1",
		Bucket:            "fry-test",
		ExpectedOwner:     "123456789012",
		RetirementProfile: "retire",
	}
	for _, tc := range []struct {
		name   string
		change func(*S3Config)
		good   bool
	}{
		{"AWS", func(c *S3Config) {}, true},
		{"missing region", func(c *S3Config) { c.Region = "" }, false},
		{"missing bucket", func(c *S3Config) { c.Bucket = "" }, false},
		{"AWS owner required", func(c *S3Config) { c.ExpectedOwner = "" }, false},
		{"bad owner", func(c *S3Config) { c.ExpectedOwner = "account" }, false},
		{"same identity label", func(c *S3Config) { c.ProducerProfile = "retire" }, false},
		{"missing retirement", func(c *S3Config) { c.RetirementProfile = "" }, false},
		{"compatible", func(c *S3Config) { c.Endpoint = "https://objects.example.test"; c.ExpectedOwner = "" }, true},
		{"plaintext remote", func(c *S3Config) { c.Endpoint = "http://objects.example.test" }, false},
		{"plaintext loopback still explicit", func(c *S3Config) { c.Endpoint = "http://127.0.0.1:9000" }, false},
		{"URL user", func(c *S3Config) { c.Endpoint = "https://u:p@example.test" }, false},
		{"URL query", func(c *S3Config) { c.Endpoint = "https://example.test/?x" }, false},
		{"URL empty query", func(c *S3Config) { c.Endpoint = "https://example.test/?" }, false},
		{"URL fragment", func(c *S3Config) { c.Endpoint = "https://example.test/#x" }, false},
		{"URL path", func(c *S3Config) { c.Endpoint = "https://example.test/bucket" }, false},
		{"encoded slash", func(c *S3Config) { c.Endpoint = "https://example.test/%2f" }, false},
		{"fixture", func(c *S3Config) {
			c.Endpoint = "http://127.0.0.1:9000/"
			c.LoopbackAnonymous = true
			c.UsePathStyle = true
		}, true},
		{
			"fixture IPv6",
			func(c *S3Config) { c.Endpoint = "http://[::1]:9000"; c.LoopbackAnonymous = true; c.UsePathStyle = true },
			true,
		},
		{"fixture hostname", func(c *S3Config) {
			c.Endpoint = "http://localhost:9000"
			c.LoopbackAnonymous = true
			c.UsePathStyle = true
		}, false},
		{
			"fixture missing path style",
			func(c *S3Config) { c.Endpoint = "http://127.0.0.1:9000"; c.LoopbackAnonymous = true },
			false,
		},
		{"fixture missing endpoint", func(c *S3Config) { c.LoopbackAnonymous = true; c.UsePathStyle = true }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.change(&cfg)
			checkS3ConfigValidation(t, cfg, tc.good)
		})
	}
}

func checkS3ConfigValidation(t *testing.T, cfg S3Config, good bool) {
	t.Helper()
	err := cfg.Validate()
	if (err == nil) != good {
		t.Fatalf("valid=%v err=%v", good, err)
	}
	if !good {
		if _, err = cfg.Open(context.Background()); err == nil {
			t.Fatal("Open accepted invalid configuration")
		}
	}
}

func TestS3ConfigFixtureRoundTrip(t *testing.T) {
	f := s3fixture.New(s3fixture.Config{})
	defer f.Close()
	t.Setenv("AWS_PROFILE", "nonexistent-profile")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "missing"))
	cfg := S3Config{
		Region:            "sa-east-1",
		Bucket:            "fry-test",
		Endpoint:          f.Server.URL,
		UsePathStyle:      true,
		LoopbackAnonymous: true,
	}
	store, err := cfg.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("runtime configured pack")
	sum := sha256.Sum256(raw)
	key := "packs/v1/test/runtime"
	if err = store.Create(context.Background(), key, bytes.NewReader(raw), int64(len(raw)), sum); err != nil {
		t.Fatal(err)
	}
	if err = store.Create(
		context.Background(),
		key,
		bytes.NewReader(raw),
		int64(len(raw)),
		sum,
	); !errors.Is(
		err,
		ErrConflict,
	) {
		t.Fatalf("overwrite accepted: %v", err)
	}
	if err = store.Retire(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	info, err := store.Stat(context.Background(), key)
	if err != nil || !info.Retired {
		t.Fatalf("%+v %v", info, err)
	}
}

func TestS3ConfigIndependentProfilesAndEndpointIsolation(t *testing.T) {
	configureS3ProfileFixture(t)
	cfg := S3Config{
		Region:            "sa-east-1",
		Bucket:            "fry-test",
		ExpectedOwner:     "123456789012",
		ProducerProfile:   "producer",
		RetirementProfile: "retirement",
	}
	producer, retirement, err := cfg.clients(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, want  string
		credentials aws.CredentialsProvider
		endpoint    *string
	}{
		{
			"producer",
			"producer-key",
			producer.Options().Credentials,
			producer.Options().BaseEndpoint,
		},
		{
			"retirement",
			"retirement-key",
			retirement.Options().Credentials,
			retirement.Options().BaseEndpoint,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			credentials, err := tc.credentials.Retrieve(context.Background())
			if err != nil || credentials.AccessKeyID != tc.want {
				t.Fatalf("wrong credentials %q %v", credentials.AccessKeyID, err)
			}
			if tc.endpoint != nil {
				t.Fatalf("ambient endpoint survived: %s", *tc.endpoint)
			}
		})
	}
	cfg.Endpoint = "https://approved.example.test"
	producer, _, err = cfg.clients(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := aws.ToString(producer.Options().BaseEndpoint); !strings.EqualFold(got, cfg.Endpoint) {
		t.Fatalf("wrong endpoint %s", got)
	}
}

func configureS3ProfileFixture(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	credentials := filepath.Join(dir, "credentials")
	if err := os.WriteFile(
		credentials,
		[]byte(
			"[producer]\naws_access_key_id = producer-key\naws_secret_access_key = "+
				"producer-secret\n[retirement]\naws_access_key_id = retirement-key\n"+
				"aws_secret_access_key = retirement-secret\n",
		),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", credentials)
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "missing"))
	t.Setenv("AWS_ACCESS_KEY_ID", "ambient-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "ambient-secret")
	t.Setenv("AWS_ENDPOINT_URL", "http://unexpected.example.test")
	t.Setenv("AWS_ENDPOINT_URL_S3", "http://unexpected-s3.example.test")
}
