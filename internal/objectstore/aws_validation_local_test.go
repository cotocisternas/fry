package objectstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
)

func TestAWSValidationLocalExplicitOptIn(t *testing.T) {
	for mask := range 8 {
		t.Run(fmt.Sprint(mask), func(t *testing.T) {
			requireOptInCombination(t, mask)
		})
	}
	if enabled, err := validationOptIn("config", "bucket", "relative.json"); enabled || err == nil {
		t.Fatal("relative manifest accepted")
	}
}

func requireOptInCombination(t *testing.T, mask int) {
	t.Helper()
	values := [3]string{"", "", ""}
	all := [3]string{"/synthetic/config.json", "fry-validation-synthetic", "/synthetic/new-manifest.json"}
	for i := range values {
		if mask&(1<<i) != 0 {
			values[i] = all[i]
		}
	}
	enabled, err := validationOptIn(values[0], values[1], values[2])
	wantEnabled := mask == 7
	wantError := mask != 0 && mask != 7
	if enabled != wantEnabled || (err != nil) != wantError {
		t.Fatalf("mask=%d enabled=%v error=%v; want enabled=%v error=%v", mask, enabled, err, wantEnabled, wantError)
	}
}

func TestAWSValidationLocalRejectsUnsafeLiveConfiguration(t *testing.T) {
	base := S3Config{
		Region:            "sa-east-1",
		Bucket:            "fry-validation-synthetic",
		ExpectedOwner:     "123456789012",
		ProducerProfile:   "synthetic-producer",
		RetirementProfile: "synthetic-retirement",
	}
	if err := validateLiveConfig(base, base.Bucket); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*S3Config){
		"other region":     func(c *S3Config) { c.Region = "us-east-1" },
		"missing owner":    func(c *S3Config) { c.ExpectedOwner = "" },
		"malformed owner":  func(c *S3Config) { c.ExpectedOwner = "owner" },
		"ordinary bucket":  func(c *S3Config) { c.Bucket = "business-mail" },
		"dotted bucket":    func(c *S3Config) { c.Bucket = "fry-validation-not.allowed" },
		"oversized bucket": func(c *S3Config) { c.Bucket = "fry-validation-" + strings.Repeat("a", 64) },
		"endpoint":         func(c *S3Config) { c.Endpoint = "https://127.0.0.1:9999" },
		"anonymous": func(c *S3Config) {
			c.Endpoint = "http://127.0.0.1:9999"
			c.UsePathStyle = true
			c.LoopbackAnonymous = true
		},
		"path style":          func(c *S3Config) { c.UsePathStyle = true },
		"implicit producer":   func(c *S3Config) { c.ProducerProfile = "" },
		"implicit retirement": func(c *S3Config) { c.RetirementProfile = "" },
		"same profile":        func(c *S3Config) { c.RetirementProfile = c.ProducerProfile },
	} {
		t.Run(name, func(t *testing.T) {
			c := base
			change(&c)
			if err := validateLiveConfig(c, c.Bucket); err == nil {
				t.Fatal("unsafe live configuration accepted")
			}
		})
	}
	if err := validateLiveConfig(base, base.Bucket+"-different"); err == nil {
		t.Fatal("inexact write acknowledgment accepted")
	}
}

type validationHTTPError struct {
	error
	status int
}

func (e validationHTTPError) HTTPStatusCode() int { return e.status }
func (e validationHTTPError) Unwrap() error       { return e.error }
func TestAWSValidationLocalAccessDeniedRequiresCodeAndStatus(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		status     int
		good       bool
	}{
		{
			"correct",
			"AccessDenied",
			403,
			true,
		},
		{
			"precondition",
			"PreconditionFailed",
			412,
			false,
		},
		{
			"wrong403",
			"InvalidAccessKeyId",
			403,
			false,
		},
		{
			"wrong status",
			"AccessDenied",
			412,
			false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := fmt.Errorf(
				"operation: %w",
				validationHTTPError{&smithy.GenericAPIError{Code: tc.code, Message: "synthetic"}, tc.status},
			)
			if got := requireAccessDenied(err) == nil; got != tc.good {
				t.Fatalf("accepted=%v want%v", got, tc.good)
			}
		})
	}
	for _, err := range []error{nil, errors.New("403 AccessDenied"), &smithy.GenericAPIError{Code: "AccessDenied"}} {
		if requireAccessDenied(err) == nil {
			t.Fatal("unproven HTTP denial accepted")
		}
	}
}

func TestAWSValidationLocalManifestExclusivePrivateAndComplete(t *testing.T) {
	m := validationManifest{
		Version:       1,
		Bucket:        "fry-validation-synthetic",
		ExpectedOwner: "123456789012",
		Region:        "sa-east-1",
		Namespace:     "synthetic-namespace",
		ProducerARN:   "arn:aws:sts::123456789012:assumed-role/producer/session",
		RetirementARN: "arn:aws:sts::123456789012:assumed-role/retirement/session",
		RunID:         "synthetic-run",
		Keys:          []string{"pack/one", "pack/two", "pack/marker"},
	}
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := writeValidationManifest(path, m); err != nil {
		t.Fatal(err)
	}
	// Fixed manifest.json under t.TempDir; verifies saved cleanup authority; owner: FRY maintainers; expires:
	// 2027-01-18.
	// #nosec G304 -- See the path-safety rationale above.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	requirePrivateManifestMode(t, path)
	requireManifestContents(t, raw, m)
	if err := writeValidationManifest(path, validationManifest{}); err == nil {
		t.Fatal("existing cleanup manifest overwritten")
	}
	// Fixed manifest.json under t.TempDir; verifies exclusive write preserved existing authority; owner: FRY
	// maintainers; expires: 2027-01-18.
	// #nosec G304 -- See the path-safety rationale above.
	again, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, again) {
		t.Fatal("existing cleanup authority changed")
	}
	if err := writeValidationManifest("relative-manifest.json", m); err == nil {
		t.Fatal("relative output accepted")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := writeValidationManifest(link, m); err == nil {
		t.Fatal("symlink output accepted")
	}
}

func requirePrivateManifestMode(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("manifest permissions=%o", info.Mode().Perm())
	}
}

func requireManifestContents(t *testing.T, raw []byte, m validationManifest) {
	t.Helper()
	var restored validationManifest
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m, restored) {
		t.Fatalf("manifest lost cleanup authority: %+v", restored)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	allowed := []string{
		"version",
		"bucket",
		"expected_bucket_owner",
		"region",
		"namespace",
		"producer_arn",
		"retirement_arn",
		"run_id",
		"keys",
	}
	if len(fields) != len(allowed) {
		t.Fatalf("unexpected manifest fields: %v", fields)
	}
	for _, name := range allowed {
		if _, ok := fields[name]; !ok {
			t.Fatalf("missing manifest field %s", name)
		}
	}
}

func TestAWSValidationLocalStableRoleIdentityAndTemporaryCredentials(t *testing.T) {
	now := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	credentials := aws.Credentials{
		AccessKeyID:     "SYNTHETIC_ACCESS",
		SecretAccessKey: "SYNTHETIC_SECRET",
		SessionToken:    "SYNTHETIC_TOKEN",
		CanExpire:       true,
		Expires:         now.Add(time.Hour),
	}
	identity := func(session string) *sts.GetCallerIdentityOutput {
		return &sts.GetCallerIdentityOutput{
			Account: aws.String("123456789012"),
			Arn:     aws.String("arn:aws:sts::123456789012:assumed-role/producer/" + session),
			UserId:  aws.String("AROASYNTHETIC:" + session),
		}
	}
	first, err := validateValidationPrincipal("123456789012", credentials, identity("first"), now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := validateValidationPrincipal("123456789012", credentials, identity("second"), now)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || first != "AROASYNTHETIC" {
		t.Fatal("same role sessions acquired distinct role identities")
	}
	for name, mutate := range map[string]func(*aws.Credentials, *sts.GetCallerIdentityOutput){
		"long lived": func(c *aws.Credentials, _ *sts.GetCallerIdentityOutput) { c.SessionToken = "" },
		"expiring":   func(c *aws.Credentials, _ *sts.GetCallerIdentityOutput) { c.Expires = now.Add(time.Minute) },
		"other account": func(_ *aws.Credentials, i *sts.GetCallerIdentityOutput) {
			i.Account = aws.String("999999999999")
		},
		"iam user": func(_ *aws.Credentials, i *sts.GetCallerIdentityOutput) {
			i.Arn = aws.String("arn:aws:iam::123456789012:user/test")
		},
		"missing stable id": func(_ *aws.Credentials, i *sts.GetCallerIdentityOutput) {
			i.UserId = aws.String("session")
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := credentials
			i := identity("test")
			mutate(&c, i)
			if _, err := validateValidationPrincipal("123456789012", c, i, now); err == nil {
				t.Fatal("invalid principal accepted")
			}
		})
	}
}

func TestAWSValidationLocalConfigFileBoundaries(t *testing.T) {
	dir := t.TempDir()
	good := []byte(
		"{\"region\":\"sa-east-1\",\"bucket\":\"fry-validation-synthetic\",\"ex" +
			"pected_bucket_owner\":\"123456789012\",\"producer_profile\":\"producer" +
			"\",\"retirement_profile\":\"retirement\"}",
	)
	path := filepath.Join(dir, "valid.json")
	if err := os.WriteFile(path, good, 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := readValidationConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	want := S3Config{
		Region:            "sa-east-1",
		Bucket:            "fry-validation-synthetic",
		ExpectedOwner:     "123456789012",
		ProducerProfile:   "producer",
		RetirementProfile: "retirement",
	}
	if config != want {
		t.Fatalf("configuration changed: %+v", config)
	}
	for name, raw := range map[string][]byte{
		"unknown":   []byte(`{"unknown":true}`),
		"trailing":  append(append([]byte(nil), good...), []byte(" {}")...),
		"malformed": []byte("{"),
		"oversized": bytes.Repeat([]byte(" "), (16<<10)+1),
	} {
		t.Run(name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(file, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := readValidationConfig(file); err == nil {
				t.Fatal("invalid configuration file accepted")
			}
		})
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	// No writer opens the FIFO: an implementation that opens before checking its
	// type would block, so retain a bounded test-level fail-safe without leaving a
	// blocked goroutine behind. O_RDWR is opened only after the deadline to unblock.
	for name, file := range map[string]string{
		"fifo":      fifo,
		"directory": dir,
		"symlink":   link,
		"missing":   filepath.Join(dir, "missing"),
	} {
		t.Run(name, func(t *testing.T) {
			requireNonregularConfigRejected(t, name, file, fifo)
		})
	}
}

func requireNonregularConfigRejected(t *testing.T, name, file, fifo string) {
	t.Helper()
	done := make(chan error, 1)
	go func() { _, err := readValidationConfig(file); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("nonregular configuration accepted")
		}
	case <-time.After(time.Second):
		rescueConfigFIFO(t, name, fifo)
		select {
		case readErr := <-done:
			if readErr != nil {
				t.Log("rescued read", readErr)
			}
		case <-time.After(time.Second):
			t.Error("configuration reader did not join after unblock")
		}
		t.Fatal("configuration validation blocked on file type")
	}
}

func rescueConfigFIFO(t *testing.T, name, fifo string) {
	t.Helper()
	var unblock *os.File
	if name == "fifo" {
		var openErr error
		// Fixed FIFO under t.TempDir; nonblocking rescue joins the test reader; owner: FRY maintainers; expires:
		// 2027-01-18.
		// #nosec G304 -- See the path-safety rationale above.
		unblock, openErr = os.OpenFile(fifo, os.O_RDWR|syscall.O_NONBLOCK, 0)
		if openErr != nil {
			t.Error("FIFO rescue open", openErr)
		}
		if unblock != nil {
			if _, writeErr := unblock.Write([]byte("{}")); writeErr != nil {
				t.Error("FIFO rescue write", writeErr)
			}
			if closeErr := unblock.Close(); closeErr != nil {
				t.Error("FIFO rescue close", closeErr)
			}
		}
	}
}
