package imapsource

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
)

func TestSearchExpansionBound(t *testing.T) {
	_, err := boundedUIDs(&imap.SearchData{All: imap.UIDSet{{Start: 1, Stop: 4294967295}}})
	if err == nil {
		t.Fatal("unbounded UID expansion")
	}
	_, err = boundedUIDs(&imap.SearchData{All: imap.UIDSet{{Start: 0, Stop: 1}}})
	if err == nil {
		t.Fatal("dynamic search result accepted")
	}
	uids, err := boundedUIDs(&imap.SearchData{All: imap.UIDSetNum(1, 3, 9)})
	if err != nil || len(uids) != 3 {
		t.Fatal(uids, err)
	}
}

func TestCollectionPolicies(t *testing.T) {
	var mutations []string
	r := collectionScript(t, &mutations)
	if _, err := r.collection.Select("INBOX"); err != nil {
		t.Fatal(err)
	}
	uids, err := r.collection.Search()
	if err != nil || len(uids) != 2 {
		t.Fatal(uids, err)
	}
	m, found, err := r.collection.Metadata(7)
	if err != nil || !found || !m.InternalDate.IsZero() || len(m.Flags) != 2 {
		t.Fatal(m, found, err)
	}
}

func TestCleanupIdentity(t *testing.T) {
	var mutations []string
	r := collectionScript(t, &mutations)
	if _, err := r.collection.Select("INBOX"); err != nil {
		t.Fatal(err)
	}
	for _, identity := range []SourceMessage{{"Other", 4, 7}, {"INBOX", 5, 7}, {"INBOX", 4, 0}} {
		if err := r.cleanup.Delete(identity); err == nil {
			t.Fatal("invalid cleanup authorized", identity)
		}
	}
	if err := r.cleanup.Delete(SourceMessage{"inbox", 4, 7}); err != nil {
		t.Fatal(err)
	}
	if len(mutations) != 2 || !strings.Contains(mutations[0], "UID STORE 7 +FLAGS.SILENT (\\Deleted)") ||
		!strings.Contains(mutations[1], "UID EXPUNGE 7") {
		t.Fatal(mutations)
	}
}

func TestCollectionDefaultCapability(t *testing.T) {
	r := scriptPolicy(t, func(tag, cmd string) string {
		if !strings.HasPrefix(cmd, "EXAMINE ") {
			t.Errorf("unexpected command %s", cmd)
		}
		return fmt.Sprintf("* 0 EXISTS\r\n* OK [UIDVALIDITY 4] epoch\r\n%s OK selected\r\n", tag)
	}, true, false)
	if r.cleanup != nil {
		t.Fatal("default cleanup capability")
	}
	if _, err := r.collection.Select("INBOX"); err != nil {
		t.Fatal(err)
	}
}

func collectionScript(t *testing.T, mutations *[]string) scriptedReader {
	t.Helper()
	return scriptPolicy(t, func(tag, cmd string) string {
		switch {
		case strings.HasPrefix(cmd, "SELECT "):
			return fmt.Sprintf("* 1 EXISTS\r\n* OK [UIDVALIDITY 4] epoch\r\n%s OK selected\r\n", tag)
		case strings.Contains(cmd, "UID SEARCH"):
			return fmt.Sprintf("* SEARCH 7 7 9\r\n%s OK search\r\n", tag)
		case strings.Contains(cmd, "RFC822.SIZE"):
			return fmt.Sprintf("* 1 FETCH (UID 7 RFC822.SIZE 3 FLAGS (\\Unknown custom))\r\n%s OK metadata\r\n", tag)
		default:
			*mutations = append(*mutations, cmd)
			return fmt.Sprintf("%s OK done\r\n", tag)
		}
	}, true, true)
}

func TestCollectionUnsolicitedLiteralIsDrained(t *testing.T) {
	r := scriptPolicy(t, func(tag, cmd string) string {
		return fmt.Sprintf("* 1 FETCH (UID 9 BODY[] {3}\r\nabc)\r\n* SEARCH 7\r\n%s OK search\r\n", tag)
	}, true, false)
	uids, err := r.collection.Search()
	if err != nil || len(uids) != 1 || uids[0] != 7 {
		t.Fatal(uids, err)
	}
}

func TestCleanupRequiresUIDPlusBeforeWork(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go serveScript(t, ln, done, func(tag, cmd string) string {
		t.Errorf("work before capability check: %s", cmd)
		return fmt.Sprintf("%s BAD unexpected\r\n", tag)
	}, "IMAP4rev1")
	defer func() { testCleanup(t)(ln.Close()); <-done }()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reader, cleanup, err := DialCollection(
		ctx,
		Config{
			Address:           ln.Addr().String(),
			Username:          "u",
			Password:          "p",
			LoopbackPlaintext: true,
			MaxMessageBytes:   100,
		},
		true,
	)
	if err == nil || reader != nil || cleanup != nil {
		t.Fatal(reader, cleanup, err)
	}
}

func TestCleanupRequiresCurrentSuccessfulSelection(t *testing.T) {
	selection := 0
	r := scriptPolicy(t, func(tag, cmd string) string {
		if !strings.HasPrefix(cmd, "SELECT ") {
			t.Errorf("unexpected mutation: %s", cmd)
			return fmt.Sprintf("%s BAD unexpected\r\n", tag)
		}
		selection++
		if selection == 3 {
			return fmt.Sprintf("%s NO failed\r\n", tag)
		}
		return fmt.Sprintf("* 0 EXISTS\r\n* OK [UIDVALIDITY %d] epoch\r\n%s OK selected\r\n", selection, tag)
	}, true, true)
	if _, err := r.collection.Select("INBOX"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.collection.Select("INBOX"); err != nil {
		t.Fatal(err)
	}
	if err := r.cleanup.Delete(SourceMessage{"INBOX", 1, 7}); err == nil {
		t.Fatal("old epoch authorized")
	}
	if _, err := r.collection.Select("INBOX"); err == nil {
		t.Fatal("selection unexpectedly succeeded")
	}
	if err := r.cleanup.Delete(SourceMessage{"INBOX", 2, 7}); err == nil {
		t.Fatal("failed selection retained cleanup authority")
	}
}

func TestUnreadBodyRevokesCleanup(t *testing.T) {
	r := scriptPolicy(t, func(tag, cmd string) string {
		if strings.HasPrefix(cmd, "SELECT ") {
			return fmt.Sprintf("* 1 EXISTS\r\n* OK [UIDVALIDITY 4] epoch\r\n%s OK selected\r\n", tag)
		}
		if strings.Contains(cmd, "BODY.PEEK[]") {
			return fmt.Sprintf("* 1 FETCH (UID 7 BODY[] {3}\r\nabc)\r\n%s OK done\r\n", tag)
		}
		t.Errorf("unexpected mutation: %s", cmd)
		return fmt.Sprintf("%s BAD unexpected\r\n", tag)
	}, true, true)
	if _, err := r.collection.Select("INBOX"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.collection.Fetch(
		7,
		3,
		func(body io.ReadCloser) error { return nil },
	); !errors.Is(
		err,
		ErrBodyUnconsumed,
	) {
		t.Fatal(err)
	}
	if err := r.cleanup.Delete(SourceMessage{"INBOX", 4, 7}); !errors.Is(err, ErrSessionClosed) {
		t.Fatal(err)
	}
}
