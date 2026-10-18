package imapsource

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// script uses a real TCP socket so literal truncation and command completion
// exercise the client's protocol reader, rather than a mocked body reader.
func script(t *testing.T, response func(string, string) string) *Reader {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, e := ln.Accept()
		if e != nil {
			return
		}
		defer func() {
			testCleanup(t)(c.Close())
		}()
		testCheck(t)(c.SetDeadline(time.Now().Add(5 * time.Second)))
		testResult(t)(fmt.Fprint(c, "* OK [CAPABILITY IMAP4rev1] ready\r\n"))
		s := bufio.NewScanner(c)
		for s.Scan() {
			line := s.Text()
			tag, rest, _ := strings.Cut(line, " ")
			if rest == "CAPABILITY" {
				testResult(t)(fmt.Fprintf(c, "* CAPABILITY IMAP4rev1\r\n%s OK caps\r\n", tag))
				continue
			}
			if strings.HasPrefix(rest, "LOGIN ") {
				testResult(t)(fmt.Fprintf(c, "%s OK login\r\n", tag))
				continue
			}
			reply := response(tag, rest)
			testResult(t)(fmt.Fprint(c, reply))
			if !strings.Contains(reply, tag+" ") {
				return
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	r, err := Dial(
		ctx,
		Config{
			Address:           ln.Addr().String(),
			Username:          "u",
			Password:          "p",
			LoopbackPlaintext: true,
			MaxMessageBytes:   100,
		},
	)
	if err != nil {
		cancel()
		testCleanup(t)(ln.Close())
		t.Fatal(err)
	}
	t.Cleanup(func() { testCleanup(t)(r.Close()); cancel(); testCleanup(t)(ln.Close()); <-done })
	return r
}

func TestFetchVerifiedCompletion(t *testing.T) {
	cases := []struct {
		name, response string
		found, good    bool
	}{
		{"trailing UID", "* 1 FETCH (BODY[] {3}\r\nabc UID 7)\r\n%s OK done\r\n", true, true},
		{"tagged NO", "* 1 FETCH (UID 7 BODY[] {3}\r\nabc)\r\n%s NO failure\r\n", false, false},
		{"truncated", "* 1 FETCH (UID 7 BODY[] {3}\r\na", false, false},
		{"missing", "%s OK done\r\n", false, true},
		{"wrong UID", "* 1 FETCH (BODY[] {3}\r\nabc UID 8)\r\n%s OK done\r\n", false, false},
		{"duplicate literal", "* 1 FETCH (UID 7 BODY[] {3}\r\nabc BODY[] {3}\r\ndef)\r\n%s OK done\r\n", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := script(t, func(tag, cmd string) string {
				if !strings.Contains(cmd, "BODY.PEEK[]") {
					t.Errorf("missing PEEK: %s", cmd)
				}
				return strings.ReplaceAll(tc.response, "%s", tag)
			})
			var data []byte
			found, err := r.Fetch(7, 3, func(body io.ReadCloser) error {
				defer func() {
					testCleanup(t)(body.Close())
				}()
				var e error
				data, e = io.ReadAll(body)
				return e
			})
			if found != tc.found || (err == nil) != tc.good {
				t.Fatalf("found=%v err=%v", found, err)
			}
			if found && string(data) != "abc" {
				t.Fatal(string(data))
			}
		})
	}
}

func TestSearchWindow(t *testing.T) {
	for _, tc := range []struct {
		name, result string
		good         bool
	}{{"valid", "7 9", true}, {"duplicate", "7 7", false}, {"outside", "11", false}, {"zero", "0", false}} {
		t.Run(tc.name, func(t *testing.T) {
			r := script(t, func(tag, cmd string) string {
				if !strings.Contains(cmd, "UID SEARCH UID 7:10") {
					t.Errorf("unexpected command %s", cmd)
				}
				return fmt.Sprintf("* SEARCH %s\r\n%s OK done\r\n", tc.result, tag)
			})
			_, err := r.SearchWindow(7, 10)
			if (err == nil) != tc.good {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestLocalLimits(t *testing.T) {
	r := &Reader{maxBytes: 10}
	for _, v := range [][2]uint32{{0, 1}, {2, 1}, {1, 1025}} {
		if _, err := r.SearchWindow(v[0], v[1]); err == nil {
			t.Fatal("accepted invalid window")
		}
	}
	if _, err := Dial(
		context.Background(),
		Config{Address: "localhost:143", Username: "u", Password: "p", LoopbackPlaintext: true, MaxMessageBytes: 1},
	); err == nil {
		t.Fatal("accepted hostname plaintext")
	}
}

func TestMetadataPresence(t *testing.T) {
	for _, tc := range []struct {
		name, items string
		good        bool
	}{
		{"complete", "UID 7 FLAGS (\\Seen tag) INTERNALDATE \"17-Oct-2026 12:00:00 +0000\" RFC822.SIZE 3", true},
		{"missing size", "UID 7 FLAGS () INTERNALDATE \"17-Oct-2026 12:00:00 +0000\"", false},
		{"missing flags", "UID 7 INTERNALDATE \"17-Oct-2026 12:00:00 +0000\" RFC822.SIZE 3", false},
		{"missing date", "UID 7 FLAGS () RFC822.SIZE 3", false},
		{"oversized", "UID 7 FLAGS () INTERNALDATE \"17-Oct-2026 12:00:00 +0000\" RFC822.SIZE 101", false},
		{
			"duplicate size",
			"UID 7 FLAGS () INTERNALDATE \"17-Oct-2026 12:00:00 +0000\" RFC822.SIZE 3 RFC822.SIZE 3",
			false,
		},
		{
			"unknown system flag",
			"UID 7 FLAGS (\\Unknown) INTERNALDATE \"17-Oct-2026 12:00:00 +0000\" RFC822.SIZE 3",
			false,
		},
		{"unexpected item", "UID 7 FLAGS () INTERNALDATE \"17-Oct-2026 12:00:00 +0000\" RFC822.SIZE 3 " +
			"ENVELOPE (NIL NIL NIL NIL NIL NIL NIL NIL NIL NIL)", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := script(
				t,
				func(tag, cmd string) string { return fmt.Sprintf("* 1 FETCH (%s)\r\n%s OK done\r\n", tc.items, tag) },
			)
			_, found, err := r.Metadata(7)
			if found != tc.good || (err == nil) != tc.good {
				t.Fatalf("found=%v err=%v", found, err)
			}
		})
	}
}

func TestMetadataDoesNotCopySessionRecentFlag(t *testing.T) {
	r := script(t, func(tag, cmd string) string {
		return fmt.Sprintf(
			"* 1 FETCH (UID 7 FLAGS (\\Recent \\Seen custom) INTERNALDATE \"17-Oct-2026 "+
				"12:00:00 +0000\" RFC822.SIZE 3)\r\n%s OK done\r\n",
			tag,
		)
	})
	metadata, found, err := r.Metadata(7)
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if strings.Join(metadata.Flags, ",") != "\\Seen,custom" {
		t.Fatalf("persisted flags: %v", metadata.Flags)
	}
}

func TestDiscoveryBounds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		count int
		good  bool
	}{{"limit", MaxFolders, true}, {"over limit", MaxFolders + 1, false}} {
		t.Run(tc.name, func(t *testing.T) {
			r := script(t, func(tag, cmd string) string {
				var b strings.Builder
				for i := range tc.count {
					testResult(t)(fmt.Fprintf(&b, "* LIST () \".\" \"folder%d\"\r\n", i))
				}
				testResult(t)(fmt.Fprintf(&b, "%s OK done\r\n", tag))
				return b.String()
			})
			r.omitSubscriptions = true
			folders, err := r.Discover()
			if (err == nil) != tc.good {
				t.Fatalf("err=%v", err)
			}
			if tc.good && (len(folders) != tc.count || folders[0].Delimiter != '.') {
				t.Fatal("discovery lost folders or delimiter")
			}
		})
	}
}

func TestEarlyCloseCancels(t *testing.T) {
	r := script(t, func(tag, cmd string) string {
		return fmt.Sprintf("* 1 FETCH (UID 7 BODY[] {3}\r\nabc)\r\n%s OK done\r\n", tag)
	})
	found, err := r.Fetch(7, 3, func(body io.ReadCloser) error { return body.Close() })
	if found || err == nil {
		t.Fatalf("found=%v err=%v", found, err)
	}
}

func TestDiscoverySubscriptions(t *testing.T) {
	r := script(t, func(tag, cmd string) string {
		if strings.HasPrefix(cmd, "LIST ") {
			return fmt.Sprintf(
				"* LIST () \".\" \"Projects.Work\"\r\n* LIST (\\Noselect) \".\" \"Parent\"\r\n"+
					"%s OK list\r\n",
				tag,
			)
		}
		if strings.HasPrefix(cmd, "LSUB ") {
			return fmt.Sprintf(
				"* LSUB () \".\" \"Projects.Work\"\r\n* LSUB () \".\" \"Ghost\"\r\n"+
					"%s OK subscriptions\r\n",
				tag,
			)
		}
		t.Errorf("unexpected %s", cmd)
		return fmt.Sprintf("%s BAD command\r\n", tag)
	})
	folders, err := r.Discover()
	if err != nil || len(folders) != 1 || !folders[0].Subscribed || folders[0].Name != "Projects.Work" ||
		folders[0].Delimiter != '.' {
		t.Fatalf("%+v %v", folders, err)
	}
}
