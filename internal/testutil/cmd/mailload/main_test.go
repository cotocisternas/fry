package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

func TestSampleVerificationRejectsCorruptOrUnexpectedMail(t *testing.T) {
	for _, name := range []string{
		"valid",
		"body",
		"size",
		"date",
		"flags",
		"unaccepted",
		"index",
		"run",
		"malformed",
	} {
		t.Run(name, func(t *testing.T) {
			raw := message("fixture", 0, 1024)
			metadata := &imapclient.FetchMessageBuffer{RFC822Size: 1024, InternalDate: time.Now()}
			accepted := []bool{true}
			corruptSample(name, &raw, metadata, accepted)
			err := verifySampleBody(metadata, raw, 7, config{size: 1024}, "fixture", accepted)
			if (err == nil) != (name == "valid") {
				t.Fatalf("sample verification: %v", err)
			}
		})
	}
}

func corruptSample(name string, raw *[]byte, metadata *imapclient.FetchMessageBuffer, accepted []bool) {
	switch name {
	case "body":
		(*raw)[len(*raw)-3] ^= 1
	case "size":
		metadata.RFC822Size--
	case "date":
		metadata.InternalDate = time.Time{}
	case "flags":
		metadata.Flags = []imap.Flag{imap.FlagSeen}
	case "unaccepted":
		accepted[0] = false
	case "index":
		*raw = message("fixture", 1, 1024)
	case "run":
		*raw = message("different", 0, 1024)
	case "malformed":
		*raw = []byte("invalid header\r\n\r\n")
	}
}

func TestWorkloadRequiresBoundedNumericLoopback(t *testing.T) {
	valid := config{
		smtp:        "127.0.0.1:2525",
		imap:        "[::1]:1143",
		user:        "fixture",
		password:    "synthetic",
		count:       1000,
		size:        65536,
		concurrency: 16,
		timeout:     time.Minute,
	}
	if err := validate(valid); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{
		"localhost:25",
		"0.0.0.0:25",
		"192.168.1.1:25",
		"8.8.8.8:25",
		"[::]:25",
		"127.0.0.1:0",
	} {
		bad := valid
		bad.smtp = endpoint
		if err := validate(bad); err == nil {
			t.Fatalf("unsafe SMTP endpoint accepted: %s", endpoint)
		}
		bad = valid
		bad.imap = endpoint
		if err := validate(bad); err == nil {
			t.Fatalf("unsafe IMAP endpoint accepted: %s", endpoint)
		}
	}
	for _, mutate := range []func(*config){
		func(c *config) { c.count = 50001 },
		func(c *config) { c.count = 50000; c.size = 1 << 20 },
		func(c *config) { c.concurrency = 129 },
		func(c *config) { c.timeout = 3 * time.Minute },
	} {
		bad := valid
		mutate(&bad)
		if err := validate(bad); err == nil {
			t.Fatal("unbounded workload accepted")
		}
	}
}

func TestSyntheticMessageExactSizeHashAndSMTPLineBounds(t *testing.T) {
	for _, size := range []int{1024, 65536, 1 << 20} {
		checkSyntheticMessage(t, size)
	}
}

// failingDATAConn lets the SMTP handshake finish, then fails a body flush.
// A subsequent write could finalize a partial message on a real transport.
type failingDATAConn struct {
	net.Conn
	responses          *strings.Reader
	dataStarted        bool
	failed             bool
	closed             bool
	writesAfterFailure int
}

func (c *failingDATAConn) Read(p []byte) (int, error) { return c.responses.Read(p) }
func (c *failingDATAConn) Write(p []byte) (int, error) {
	if c.failed {
		c.writesAfterFailure++
		return 0, io.ErrClosedPipe
	}
	if c.dataStarted {
		c.failed = true
		return 0, io.ErrClosedPipe
	}
	if bytes.Equal(p, []byte("DATA\r\n")) {
		c.dataStarted = true
	}
	return len(p), nil
}
func (c *failingDATAConn) Close() error { c.closed = true; return nil }

func TestDeliverAbortsTransportWithoutFinishingFailedDATA(t *testing.T) {
	for _, size := range []int{1024, 65536} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			checkFailedDATA(t, size)
		})
	}
}

func checkSyntheticMessage(t *testing.T, size int) {
	t.Helper()
	raw := message("0123456789abcdef", 1234, size)
	if len(raw) != size {
		t.Fatal("incorrect raw size", len(raw), size)
	}
	parsed, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(parsed.Body)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	if parsed.Header.Get("X-Fry-Load-SHA256") != hex.EncodeToString(digest[:]) {
		t.Fatal("body hash mismatch")
	}
	for line := range bytes.SplitSeq(raw, []byte("\r\n")) {
		if len(line) > 998 {
			t.Fatal("oversized SMTP line", len(line))
		}
	}
	if bytes.Equal(raw, message("0123456789abcdef", 1235, size)) {
		t.Fatal("indices did not distinguish messages")
	}
}

func checkFailedDATA(t *testing.T, size int) {
	t.Helper()
	connection := &failingDATAConn{
		responses: strings.NewReader(
			"220 fixture ready\r\n250 fixture\r\n250 sender\r\n250 recipient\r\n354 send data\r\n",
		),
	}
	client, err := smtp.NewClient(connection, "localhost")
	if err != nil {
		t.Fatal(err)
	}
	err = deliver(client, "invoices@example.test", message("0123456789abcdef", 0, size))
	if err == nil {
		t.Fatal("failed DATA was accepted")
	}
	// A large body fails during Write; a buffered body fails when
	// finalizing DATA, where net/smtp can return a response error.
	if size > 4096 && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("expected body write failure, got %v", err)
	}
	if !connection.failed || !connection.closed {
		t.Fatal("failed DATA did not close its transport")
	}
	if connection.writesAfterFailure != 0 {
		t.Fatal("failed DATA attempted further writes")
	}
}
