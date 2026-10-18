package objectstore

import (
	"context"
	"testing"

	"github.com/cotocisternas/fry/internal/testutil/s3fixture"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestS3NamespaceCanonicalAddressAndCredentialIndependence(t *testing.T) {
	options := s3.Options{
		Region:       "sa-east-1",
		BaseEndpoint: aws.String("https://OBJECTS.example.test:443/"),
		UsePathStyle: true,
	}
	identity := func(opts s3.Options, bucket, owner string) string {
		t.Helper()
		namespace, err := clientNamespace(s3.New(opts), bucket, owner)
		if err != nil {
			t.Fatal(err)
		}
		return namespace
	}
	first := identity(options, "fry-test", "123456789012")
	same := options
	same.BaseEndpoint = aws.String("https://objects.example.test")
	same.Credentials = aws.AnonymousCredentials{}
	if got := identity(same, "fry-test", "123456789012"); got != first {
		t.Fatal("cosmetic endpoint/credential changes changed namespace", first, got)
	}
	for _, change := range []func(*s3.Options){
		func(o *s3.Options) { o.Region = "us-east-1" },
		func(o *s3.Options) { o.BaseEndpoint = aws.String("https://other.example.test") },
		func(o *s3.Options) { o.UsePathStyle = false },
	} {
		altered := options
		change(&altered)
		if identity(altered, "fry-test", "123456789012") == first {
			t.Fatal("changed address kept identity")
		}
	}
	if identity(options, "other-bucket", "123456789012") == first ||
		identity(options, "fry-test", "999999999999") == first {
		t.Fatal("bucket or expected owner omitted")
	}
	for _, endpoint := range []string{
		"https://user:secret@example.test",
		"https://example.test/path",
		"https://example.test?token=secret",
		"https://:443",
		"http://objects.example.test",
	} {
		altered := options
		altered.BaseEndpoint = aws.String(endpoint)
		if _, err := clientNamespace(s3.New(altered), "fry-test", ""); err == nil {
			t.Fatal("ambiguous/unsafe endpoint accepted", endpoint)
		}
	}
}

type unknownResolver struct{ s3.EndpointResolverV2 }

func TestS3NamespaceRejectsUnknownResolversAndMismatchedClients(t *testing.T) {
	a, b := s3fixture.New(s3fixture.Config{}), s3fixture.New(s3fixture.Config{})
	defer a.Close()
	defer b.Close()
	if _, err := NewS3(context.Background(), a.Client(), b.Client(), "fry-test", ""); err == nil {
		t.Fatal("producer/retirement endpoint mismatch accepted")
	}
	options := a.Client().Options()
	options.EndpointResolverV2 = unknownResolver{s3.NewDefaultEndpointResolverV2()}
	if _, err := NewS3(context.Background(), s3.New(options), a.Client(), "fry-test", ""); err == nil {
		t.Fatal("custom resolver silently identified by BaseEndpoint")
	}
	if len(a.Requests()) != 0 || len(b.Requests()) != 0 {
		t.Fatal("namespace rejected only after contacting mismatched stores")
	}
}

func TestS3NamespaceFreezesCallerEndpointPointer(t *testing.T) {
	fixture := s3fixture.New(s3fixture.Config{})
	defer fixture.Close()
	endpoint := fixture.Server.URL
	options := fixture.Client().Options()
	options.BaseEndpoint = &endpoint
	client := s3.New(options)
	store, err := NewS3(context.Background(), client, client, "fry-test", "")
	if err != nil {
		t.Fatal(err)
	}
	identity := store.Namespace()
	endpoint = "http://127.0.0.1:1"
	if _, err = store.Stat(context.Background(), "packs/v1/test/missing"); err == nil {
		t.Fatal("missing object unexpectedly exists")
	}
	if got := aws.ToString(store.producer.Options().BaseEndpoint); got != fixture.Server.URL ||
		store.Namespace() != identity {
		t.Fatal("caller redirected a bound adapter", got)
	}
}
