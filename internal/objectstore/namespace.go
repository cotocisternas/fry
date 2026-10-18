package objectstore

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"reflect"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Namespace is a versioned physical object address, independent of credentials
// and profile labels. A changed identity cannot be adopted by an existing store.
func (s *S3) Namespace() string { return s.namespace }

type s3Namespace struct {
	Version       int    `json:"version"`
	Provider      string `json:"provider"`
	Endpoint      string `json:"endpoint"`
	Region        string `json:"region"`
	PathStyle     bool   `json:"path_style"`
	Bucket        string `json:"bucket"`
	ExpectedOwner string `json:"expected_owner"`
}

func clientNamespace(client *s3.Client, bucket, owner string) (string, error) {
	o := client.Options()
	if err := validateEndpointRouting(o); err != nil {
		return "", err
	}
	if o.Region == "" || strings.TrimSpace(o.Region) != o.Region || strings.ContainsAny(bucket, "/:\\") ||
		strings.TrimSpace(bucket) != bucket {
		return "", fmt.Errorf("objectstore: explicit region and ordinary bucket name required")
	}
	endpoint, err := namespaceEndpoint(o, owner)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(s3Namespace{1, "s3", endpoint, o.Region, o.UsePathStyle, bucket, owner})
	return string(encoded), err
}

func namespaceEndpoint(o s3.Options, owner string) (string, error) {
	if owner != "" && !accountID.MatchString(owner) {
		return "", fmt.Errorf("objectstore: expected S3 owner must be 12 digits")
	}
	endpoint := "aws"
	if o.BaseEndpoint != nil {
		var err error
		endpoint, err = canonicalEndpoint(*o.BaseEndpoint)
		if err != nil {
			return "", err
		}
	} else if owner == "" {
		return "", fmt.Errorf("objectstore: AWS expected bucket owner required")
	}
	return endpoint, nil
}

func validateEndpointRouting(o s3.Options) error {
	// Custom resolvers can choose a different destination on each operation; their
	// identity cannot be established from BaseEndpoint. Support the SDK default.
	// SA1019: reject legacy custom routing too; owner: FRY maintainers; expires: 2027-01-18.
	//nolint:staticcheck // Legacy resolver routing must be validated.
	if o.EndpointResolver != nil ||
		reflect.TypeOf(o.EndpointResolverV2) != reflect.TypeOf(s3.NewDefaultEndpointResolverV2()) {
		return fmt.Errorf("objectstore: custom endpoint resolvers cannot identify a stable namespace")
	}
	return validateRegionalRouting(o)
}

func validateRegionalRouting(o s3.Options) error {
	if (o.EndpointOptions.ResolvedRegion != "" && o.EndpointOptions.ResolvedRegion != o.Region) || o.UseARNRegion ||
		o.UseAccelerate ||
		// SA1019: legacy dual-stack routing still changes namespace.
		// Owner: FRY maintainers; expires: 2027-01-18.
		o.UseDualstack || //nolint:staticcheck // Legacy routing must be validated.
		o.EndpointOptions.DisableHTTPS ||
		o.EndpointOptions.UseDualStackEndpoint == aws.DualStackEndpointStateEnabled ||
		o.EndpointOptions.UseFIPSEndpoint == aws.FIPSEndpointStateEnabled {
		return fmt.Errorf("objectstore: special S3 endpoint routing is unsupported")
	}
	return nil
}

func canonicalEndpoint(raw string) (string, error) {
	endpoint, err := parseEndpoint(raw)
	if err != nil {
		return "", err
	}
	endpoint.Scheme = strings.ToLower(endpoint.Scheme)
	host := strings.ToLower(endpoint.Hostname())
	ip := net.ParseIP(host)
	if host == "" {
		return "", fmt.Errorf("objectstore: endpoint hostname required")
	}
	if endpoint.Scheme != "https" && (endpoint.Scheme != "http" || ip == nil || !ip.IsLoopback()) {
		return "", fmt.Errorf("objectstore: namespace endpoint requires HTTPS or numeric loopback HTTP")
	}
	return endpointAuthority(endpoint, host, ip)
}

func parseEndpoint(raw string) (*url.URL, error) {
	endpoint, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("objectstore: endpoint cannot identify a stable namespace")
	}
	if endpoint.Host == "" || endpoint.Opaque != "" || endpoint.User != nil || endpointHasSuffix(endpoint) {
		return nil, fmt.Errorf("objectstore: endpoint cannot identify a stable namespace")
	}
	return endpoint, nil
}

func endpointHasSuffix(u *url.URL) bool {
	return u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" ||
		(u.Path != "" && u.Path != "/") ||
		u.RawPath != ""
}

func endpointAuthority(endpoint *url.URL, host string, ip net.IP) (string, error) {
	port, err := canonicalPort(endpoint.Port())
	if err != nil {
		return "", err
	}
	if ip != nil {
		host = ip.String()
	}
	if (endpoint.Scheme == "https" && port == "443") || (endpoint.Scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return endpoint.Scheme + "://" + host, nil
}

func canonicalPort(port string) (string, error) {
	if port == "" {
		return "", nil
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("objectstore: invalid endpoint port")
	}
	return strconv.Itoa(n), nil
}
