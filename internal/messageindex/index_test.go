package messageindex_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"

	"github.com/cotocisternas/fry/internal/messageindex"
)

func parse(t *testing.T, raw string) *messageindex.Part {
	t.Helper()
	p, err := messageindex.Parse(context.Background(), strings.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func body(raw string, p *messageindex.Part) string { return raw[p.BodyStart:p.End] }

func TestOriginalSpansNestedContainersAndDelimiterOwnership(t *testing.T) {
	raw := "Subject: outer\r\nContent-Type: multipart/mixed; boundary=outer\r\n" +
		"\r\npreamble\r\n--outer\r\nContent-Type: multipart/alternative; " +
		"boundary=inner\r\n\r\n--inner\r\n\r\nplain\r\n--inner \t\r\n" +
		"Content-Type: text/html\r\n\r\n<b>html</b>\r\n--inner--\r\n" +
		"epilogue\r\n--outer\r\nContent-Type: message/rfc822\r\n\r\nSubject: " +
		"nested\r\nContent-Type: text/plain\r\n\r\nforwarded\r\nsecond line\r\n" +
		"\r\n--outer\r\nContent-Type: application/pdf\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\nYWJj\r\n" +
		"--outer-is-not-a-delimiter\r\n--outer--\r\nlast epilogue"
	p := parse(t, raw)
	if p.End != int64(len(raw)) || len(p.Children) != 3 {
		t.Fatalf("root: %+v", p)
	}
	alt, msg, pdf := p.Children[0], p.Children[1], p.Children[2]
	requireNestedBodies(t, raw, alt, msg, pdf)
	for _, child := range []*messageindex.Part{p, alt, msg, msg.Message, pdf, alt.Children[0]} {
		if string(child.Header) != raw[child.HeaderStart:child.BodyStart] ||
			child.BodyLines != int64(strings.Count(body(raw, child), "\n")) {
			t.Fatalf("span/line invariant: %+v", child)
		}
	}
}

func requireNestedBodies(t *testing.T, raw string, alt, msg, pdf *messageindex.Part) {
	t.Helper()
	if len(alt.Children) != 2 || body(raw, alt.Children[0]) != "plain" || body(raw, alt.Children[1]) != "<b>html</b>" ||
		!strings.HasSuffix(body(raw, alt), "epilogue") {
		t.Fatal("nested multipart spans")
	}
	requireEmbeddedMessage(t, raw, msg)
	if body(raw, pdf) != "YWJj\r\n--outer-is-not-a-delimiter" || pdf.Encoding != "base64" {
		t.Fatal("encoded bytes changed")
	}
}

func requireEmbeddedMessage(t *testing.T, raw string, msg *messageindex.Part) {
	t.Helper()
	if msg.Message == nil || body(raw, msg.Message) != "forwarded\r\nsecond line\r\n" || msg.Message.BodyLines != 2 ||
		body(raw, msg) != string(msg.Message.Header)+body(raw, msg.Message) {
		t.Fatalf("embedded message: %+v", msg)
	}
}

func TestDigestEmptyBodiesAndLongBoundaryPadding(t *testing.T) {
	for _, newline := range []string{"\r\n", "\n"} {
		raw := strings.ReplaceAll(
			"Content-Type: multipart/digest; boundary=b\n\n--b"+strings.Repeat(
				" \t",
				1000,
			)+"\n\nSubject: inside\n\n\n--b--",
			"\n",
			newline,
		)
		p := parse(t, raw)
		if p.Children[0].MediaType != "message/rfc822" || body(raw, p.Children[0].Message) != "" {
			t.Fatal("digest defaults or empty span")
		}
	}
}

func TestRejectsMalformedAndBoundedStructure(t *testing.T) {
	cases := []string{
		"no header terminator",
		"Content-Type: broken\r\n\r\nbody",
		"Content-Type: text/plain\r\nContent-Type: application/pdf\r\n\r\nbody",
		"Content-Type: multipart/mixed; boundary=b\r\n\r\n--b--\r\n",
		"Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\n\r\nbody",
		"Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: " +
			"multipart/mixed; boundary=c\r\n\r\n--c\r\n\r\nbody\r\n--b--\r\n",
		"Content-Type: message/rfc822\r\nContent-Transfer-Encoding: base64\r\n\r\nYWJj",
		"Subject: " + strings.Repeat("x", messageindex.MaxHeaderBytes) + "\r\n\r\n",
		strings.Repeat("Content-Type: message/rfc822\r\n\r\n", messageindex.MaxDepth+2) + "\r\nbody",
		"Content-Type: multipart/mixed; boundary=b\r\n\r\n" + strings.Repeat(
			"--b\r\n\r\nx\r\n",
			messageindex.MaxParts,
		) + "--b--\r\n",
		strings.Repeat("Content-Type: message/rfc822\r\nX: "+strings.Repeat("x", 900000)+"\r\n\r\n", 10) + "\r\nbody",
	}
	for i, raw := range cases {
		if _, err := messageindex.Parse(context.Background(), strings.NewReader(raw), int64(len(raw))); err == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
	for _, size := range []int64{2, 100} {
		if _, err := messageindex.Parse(context.Background(), strings.NewReader("\r\nbody"), size); err == nil {
			t.Fatal("wrong size accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := messageindex.Parse(ctx, strings.NewReader("\r\nbody"), 6); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestWrappedSourceEOFIsNotSuccessfulCompletion(t *testing.T) {
	raw := "Subject: bounded\r\n\r\nbody"
	sourceErr := fmt.Errorf("source integrity failure: %w", io.EOF)
	source := io.MultiReader(strings.NewReader(raw), failingReader{sourceErr})
	if _, err := messageindex.Parse(t.Context(), source, int64(len(raw))); !errors.Is(err, sourceErr) {
		t.Fatalf("source error = %v, want original integrity failure", err)
	}
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

type repeatedByte struct{}

func (repeatedByte) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func TestLargeUnbrokenAttachmentDoesNotAllocateItsBody(t *testing.T) {
	if testing.Short() {
		t.Skip("200 MiB bounded allocation check")
	}
	header := "Content-Type: application/octet-stream\r\n\r\n"
	const size = 200 << 20
	r := io.MultiReader(strings.NewReader(header), io.LimitReader(repeatedByte{}, size))
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	p, err := messageindex.Parse(context.Background(), r, size+int64(len(header)))
	runtime.ReadMemStats(&after)
	if err != nil || p.End-p.BodyStart != size {
		t.Fatal(p, err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 2<<20 {
		t.Fatalf("allocated %d bytes for one large attachment", allocated)
	}
}

func TestDelimiterPaddingAcrossPrefixAndBufferBoundaries(t *testing.T) {
	for _, count := range []int{250, 251, 252, 253, 254, 255, 256, 32760, 32761, 32762, 32763, 32764} {
		padding := strings.Repeat(" ", count)
		raw := "Content-Type: multipart/mixed; boundary=b\r\n\r\n--b" + padding +
			"\r\n\r\nbody\r\n--b--" + padding + "\r\n"
		p := parse(t, raw)
		if len(p.Children) != 1 || body(raw, p.Children[0]) != "body" {
			t.Fatal("padding size", count)
		}
	}
	bogus := "--b" + strings.Repeat(" ", 252) + "\r \r\n"
	raw := "Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\n\r\n" + bogus + "body\r\n--b--\r\n"
	if got := body(raw, parse(t, raw).Children[0]); got != bogus+"body" {
		t.Fatal("interior CR treated as a delimiter")
	}
	raw = strings.Repeat("X: a\r\n", messageindex.MaxHeaderFields+1) + "\r\nbody"
	if _, err := messageindex.Parse(context.Background(), strings.NewReader(raw), int64(len(raw))); err == nil {
		t.Fatal("header field budget ignored")
	}
}

func FuzzOriginalSpanInvariants(f *testing.F) {
	for _, raw := range []string{
		"\r\nbody",
		"Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\n\r\nbody\r\n" +
			"--b--\r\n",
		"Content-Type: message/rfc822\r\n\r\nSubject: nested\r\n\r\nbody",
	} {
		f.Add(raw)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		p, err := messageindex.Parse(context.Background(), strings.NewReader(raw), int64(len(raw)))
		if err != nil {
			return
		}
		verifyPartSpans(t, raw, p)
	})
}

func requirePartSpan(t *testing.T, raw string, p *messageindex.Part) {
	t.Helper()

	if p.HeaderStart < 0 || p.BodyStart < p.HeaderStart || p.End < p.BodyStart || p.End > int64(len(raw)) {
		t.Fatalf("invalid spans: %+v", p)
	}
	if string(p.Header) != raw[p.HeaderStart:p.BodyStart] || p.BodyLines != int64(strings.Count(body(raw, p), "\n")) {
		t.Fatalf("index changed bytes or line count: %+v", p)
	}
}

func verifyPartSpans(t *testing.T, raw string, p *messageindex.Part) {
	t.Helper()
	requirePartSpan(t, raw, p)

	for _, child := range p.Children {
		if child.HeaderStart < p.BodyStart || child.End > p.End {
			t.Fatal("child outside parent")
		}
		verifyPartSpans(t, raw, child)
	}
	if p.Message != nil {
		if p.Message.HeaderStart != p.BodyStart || p.Message.End != p.End {
			t.Fatal("message outside encapsulation")
		}
		verifyPartSpans(t, raw, p.Message)
	}
}
