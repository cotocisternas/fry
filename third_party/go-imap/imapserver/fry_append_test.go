package imapserver

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

func TestFRYAppendLiteralWithholdsFinalChunk(t *testing.T) {
	for _, test := range []struct {
		name, wire, extension, want string
		size                        int64
		valid                       bool
	}{
		{"valid", "abcde\r\nz NOOP\r\n", "", "abcde", 5, true},
		{"truncated", "abc", "", "", 5, false},
		{"invalid-tail", "abcde junk\r\n", "", "", 5, false},
		{"missing-tail", "abcde", "", "", 5, false},
		{"empty", "\r\nz NOOP\r\n", "", "", 0, true},
		{"empty-invalid", " junk\r\n", "", "", 0, false},
		{"utf8-valid", "abcde)\r\nz NOOP\r\n", "UTF8", "abcde", 5, true},
		{"utf8-missing-close", "abcde\r\n", "UTF8", "", 5, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			decoder := imapwire.NewDecoder(bufio.NewReader(strings.NewReader(fmt.Sprintf("{%d}\r\n", test.size)+test.wire)), imapwire.ConnSideServer)
			literal, _, err := decoder.ExpectLiteralReader()
			if err != nil {
				t.Fatal(err)
			}
			reader := &appendLiteral{literal: literal, decoder: decoder, remaining: test.size, extension: test.extension}
			if test.size == 0 {
				err = reader.finish()
			}
			var got bytes.Buffer
			if err == nil {
				_, err = io.CopyN(&got, reader, test.size)
			}
			if test.valid {
				if err != nil || got.String() != test.want {
					t.Fatal(got.String(), err)
				}
				if _, err = reader.Read(make([]byte, 1)); err != io.EOF {
					t.Fatal("no sticky EOF", err)
				}
				var next string
				if !decoder.Atom(&next) || next != "z" {
					t.Fatal("next command was consumed", next, decoder.Err())
				}
			} else {
				if err == nil {
					t.Fatal("invalid APPEND exposed all payload bytes")
				}
				if test.size > 0 && int64(got.Len()) >= test.size {
					t.Fatal("CopyN received full payload before framing validation")
				}
				if _, again := reader.Read(make([]byte, 1)); again == nil || again == io.EOF {
					t.Fatal("framing error was not sticky", again)
				}
			}
		})
	}
}

type fryAppendSession struct {
	Session
	accepted *atomic.Int64
	copyN    bool
}

func (*fryAppendSession) Close() error                   { return nil }
func (*fryAppendSession) Poll(*UpdateWriter, bool) error { return nil }
func (s *fryAppendSession) Append(_ string, reader imap.LiteralReader, _ *imap.AppendOptions) (*imap.AppendData, error) {
	var err error
	if s.copyN {
		_, err = io.CopyN(io.Discard, reader, reader.Size())
	} else {
		_, err = io.ReadAll(reader)
	}
	if err != nil {
		return nil, err
	}
	s.accepted.Add(1)
	return &imap.AppendData{UID: 1, UIDValidity: 1}, nil
}
func TestFRYAppendTCPFramingBeforeBackendSuccess(t *testing.T) {
	for _, copyN := range []bool{false, true} {
		for _, test := range []struct {
			name, command, payload string
			valid                  bool
		}{
			{"valid", "APPEND INBOX {5}", "abcde\r\n", true},
			{"truncated", "APPEND INBOX {5}", "abc", false},
			{"bad-tail", "APPEND INBOX {5}", "abcde invalid-tail\r\n", false},
			{"empty-valid", "APPEND INBOX {0}", "\r\n", true},
			{"empty-bad-tail", "APPEND INBOX {0}", "invalid-tail\r\n", false},
			{"utf8-valid", "APPEND INBOX UTF8 (~{5}", "abcde)\r\n", true},
			{"utf8-bad-tail", "APPEND INBOX UTF8 (~{5}", "abcde\r\n", false},
		} {
			t.Run(fmt.Sprintf("%s/copyN=%v", test.name, copyN), func(t *testing.T) {
				var accepted atomic.Int64
				server := New(&Options{Caps: imap.CapSet{imap.CapIMAP4rev1: {}}, NewSession: func(*Conn) (Session, *GreetingData, error) {
					return &fryAppendSession{accepted: &accepted, copyN: copyN}, &GreetingData{PreAuth: true}, nil
				}})
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() { done <- server.Serve(listener) }()
				defer func() { server.Close(); <-done }()
				conn, err := net.Dial("tcp", listener.Addr().String())
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(3 * time.Second))
				reader := bufio.NewReader(conn)
				if _, err = reader.ReadString('\n'); err != nil {
					t.Fatal(err)
				}
				fmt.Fprintf(conn, "a %s\r\n", test.command)
				line, err := reader.ReadString('\n')
				if err != nil || !strings.HasPrefix(line, "+") {
					t.Fatal(line, err)
				}
				for _, b := range []byte(test.payload) {
					if _, err = conn.Write([]byte{b}); err != nil {
						t.Fatal(err)
					}
				}
				conn.(*net.TCPConn).CloseWrite()
				var replies strings.Builder
				for {
					line, err = reader.ReadString('\n')
					replies.WriteString(line)
					if err != nil {
						break
					}
				}
				if test.valid {
					if accepted.Load() != 1 || !strings.Contains(replies.String(), "a OK") {
						t.Fatal(accepted.Load(), replies.String())
					}
				} else {
					if accepted.Load() != 0 || strings.Contains(replies.String(), "a OK") {
						t.Fatal("invalid command accepted", accepted.Load(), replies.String())
					}
				}
			})
		}
	}
}
