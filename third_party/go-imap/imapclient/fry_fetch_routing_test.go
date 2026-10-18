package imapclient

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
)

func TestFRYStrictFetchRoutesTrailingUID(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	a.SetDeadline(time.Now().Add(3 * time.Second))
	b.SetDeadline(time.Now().Add(3 * time.Second))
	go func() {
		fmt.Fprint(b, "* PREAUTH [CAPABILITY IMAP4rev1] ready\r\n")
		s := bufio.NewScanner(b)
		for s.Scan() {
			tag, cmd, _ := strings.Cut(s.Text(), " ")
			if cmd == "CAPABILITY" {
				fmt.Fprintf(b, "* CAPABILITY IMAP4rev1\r\n%s OK caps\r\n", tag)
				continue
			}
			fmt.Fprintf(b, "* 1 FETCH (BODY[] {3}\r\nabc UID 7)\r\n%s OK done\r\n", tag)
			return
		}
	}()
	c := New(a, &Options{StrictFetchRouting: true})
	defer c.Close()
	cmd := c.Fetch(imap.UIDSetNum(7), &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{{Peek: true}}})
	m := cmd.Next()
	if m == nil {
		t.Fatal("missing message")
	}
	body, ok := m.Next().(FetchItemDataBodySection)
	if !ok {
		t.Fatal("missing body")
	}
	data, err := io.ReadAll(body.Literal)
	if err != nil || string(data) != "abc" {
		t.Fatalf("%q %v", data, err)
	}
	uid, ok := m.Next().(FetchItemDataUID)
	if !ok || uid.UID != 7 {
		t.Fatal("missing trailing UID")
	}
	if m.Next() != nil || cmd.Next() != nil {
		t.Fatal("extra item")
	}
	if err := cmd.Close(); err != nil {
		t.Fatal(err)
	}
}
