package objectstore

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3Config selects independent producer and retirement credential sources.
// Different profile labels do not prove distinct IAM principals or permissions;
// deployment policy must enforce the restrictions documented by S3.
type S3Config struct {
	Region            string `json:"region"`
	Bucket            string `json:"bucket"`
	ExpectedOwner     string `json:"expected_bucket_owner"`
	Endpoint          string `json:"endpoint"`
	UsePathStyle      bool   `json:"use_path_style"`
	ProducerProfile   string `json:"producer_profile"`
	RetirementProfile string `json:"retirement_profile"`
	// LoopbackAnonymous bypasses all AWS credential lookup for synthetic fixtures.
	LoopbackAnonymous bool `json:"loopback_anonymous"`
}

var accountID = regexp.MustCompile(`^[0-9]{12}$`)

func (c S3Config) Validate() error {
	if strings.TrimSpace(c.Region) == "" || strings.TrimSpace(c.Bucket) == "" {
		return fmt.Errorf("objectstore: S3 region and bucket required")
	}
	if c.ExpectedOwner != "" && !accountID.MatchString(c.ExpectedOwner) {
		return fmt.Errorf("objectstore: expected S3 owner must be 12 digits")
	}
	if !c.LoopbackAnonymous &&
		(strings.TrimSpace(c.RetirementProfile) == "" || c.RetirementProfile == c.ProducerProfile) {
		return fmt.Errorf("objectstore: distinct explicit retirement profile required")
	}
	if c.Endpoint == "" {
		return c.validateAWSIdentity()
	}
	return c.validateEndpoint()
}

func (c S3Config) validateAWSIdentity() error {
	if c.LoopbackAnonymous {
		return fmt.Errorf("objectstore: anonymous fixture requires endpoint")
	}
	if c.ExpectedOwner == "" {
		return fmt.Errorf("objectstore: AWS expected bucket owner required")
	}
	return nil
}

func (c S3Config) validateEndpoint() error {
	endpoint, err := parseEndpoint(c.Endpoint)
	if err != nil {
		return fmt.Errorf("objectstore: invalid S3 endpoint")
	}
	host := endpoint.Hostname()
	ip := net.ParseIP(host)
	loopback := ip != nil && ip.IsLoopback()
	if endpoint.Scheme != "https" && (endpoint.Scheme != "http" || !c.LoopbackAnonymous || !loopback) {
		return fmt.Errorf("objectstore: S3 endpoint requires HTTPS")
	}
	if c.LoopbackAnonymous && (!loopback || !c.UsePathStyle) {
		return fmt.Errorf("objectstore: anonymous fixture requires numeric loopback and path style")
	}
	return validateConfiguredPort(endpoint.Port())
}

func validateConfiguredPort(port string) error {
	if port != "" {
		if _, err := strconv.ParseUint(port, 10, 16); err != nil {
			return fmt.Errorf("objectstore: invalid endpoint port")
		}
	}
	return nil
}

func (c S3Config) clients(ctx context.Context) (*s3.Client, *s3.Client, error) {
	if err := c.Validate(); err != nil {
		return nil, nil, err
	}
	// A whole-request timeout also bounds startup policy checks. Context remains
	// authoritative for caller deadlines and cancellation.
	httpClient := &http.Client{Timeout: 30 * time.Second}
	attempts := 3
	if c.LoopbackAnonymous {
		attempts = 1
	}
	load := func(profile string) (aws.Config, error) {
		if c.LoopbackAnonymous {
			return aws.Config{
				Region:           c.Region,
				Credentials:      aws.AnonymousCredentials{},
				HTTPClient:       httpClient,
				RetryMaxAttempts: attempts,
			}, nil
		}
		options := []func(*config.LoadOptions) error{
			config.WithRegion(c.Region),
			config.WithHTTPClient(httpClient),
			config.WithRetryMaxAttempts(attempts),
			config.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
			config.WithResponseChecksumValidation(aws.ResponseChecksumValidationWhenRequired),
		}
		if profile != "" {
			options = append(options, config.WithSharedConfigProfile(profile))
		}
		return config.LoadDefaultConfig(ctx, options...)
	}
	build := func(cfg aws.Config) *s3.Client {
		return s3.NewFromConfig(cfg, func(o *s3.Options) {
			// SDK endpoint environment/shared settings must not override reviewed FRY
			// configuration. These options run after SDK endpoint resolution settings.
			o.BaseEndpoint = nil
			if c.Endpoint != "" {
				o.BaseEndpoint = aws.String(c.Endpoint)
			}
			o.UsePathStyle = c.UsePathStyle
			o.RetryMaxAttempts = attempts
			o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
			o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
		})
	}
	producer, err := load(c.ProducerProfile)
	if err != nil {
		return nil, nil, err
	}
	retirement, err := load(c.RetirementProfile)
	if err != nil {
		return nil, nil, err
	}
	return build(producer), build(retirement), nil
}

// Open authenticates lazily through SDK providers and validates bucket policy
// observations through NewS3. It does not attest effective IAM separation.
func (c S3Config) Open(ctx context.Context) (*S3, error) {
	producer, retirement, err := c.clients(ctx)
	if err != nil {
		return nil, err
	}
	return NewS3(ctx, producer, retirement, c.Bucket, c.ExpectedOwner)
}
