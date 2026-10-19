package imapclient

import (
	"bufio"
	"errors"
	"io"
	"net"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

func TestFRYCommandCompletionPublishesState(t *testing.T) {
	tests := []struct {
		name    string
		newCmd  func() command
		initial imap.ConnState
		want    imap.ConnState
	}{
		{"authenticate", func() command { return &authenticateCommand{} }, imap.ConnStateNotAuthenticated, imap.ConnStateAuthenticated},
		{"login", func() command { return &loginCommand{} }, imap.ConnStateNotAuthenticated, imap.ConnStateAuthenticated},
		{"select", func() command {
			return &SelectCommand{mailbox: "INBOX", data: imap.SelectData{NumMessages: 3, Flags: []imap.Flag{imap.FlagSeen}}}
		}, imap.ConnStateAuthenticated, imap.ConnStateSelected},
		{"unauthenticate", func() command { return &unauthenticateCommand{} }, imap.ConnStateAuthenticated, imap.ConnStateNotAuthenticated},
		{"unselect", func() command { return &unselectCommand{} }, imap.ConnStateSelected, imap.ConnStateAuthenticated},
		{"logout", func() command { return &logoutCommand{} }, imap.ConnStateSelected, imap.ConnStateLogout},
	}
	for _, tt := range tests {
		for _, fail := range []bool{false, true} {
			name := tt.name + "/success"
			if fail {
				name = tt.name + "/failure"
			}
			t.Run(name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					c := &Client{state: tt.initial, enabled: imap.CapSet{imap.CapUTF8Accept: {}}}
					if tt.initial == imap.ConnStateSelected {
						c.mailbox = &SelectedMailbox{Name: "old", NumMessages: 7}
					}
					wantState := tt.want
					var wantMailbox *SelectedMailbox
					if wantState == imap.ConnStateSelected {
						wantMailbox = &SelectedMailbox{Name: "INBOX", NumMessages: 3, Flags: []imap.Flag{imap.FlagSeen}}
					}
					var cmdErr error
					if fail {
						cmdErr = errors.New("command rejected")
						wantState, wantMailbox = tt.initial, c.mailbox
					}

					cmd := tt.newCmd()
					// Stop completion exactly at its result notification. This makes
					// the state visible before a waiter can return observable without
					// depending on which goroutine the scheduler runs after a send.
					cmd.base().done = make(chan error)
					contReq := c.registerContReq(cmd)
					go c.completeCommand(cmd, cmdErr)
					synctest.Wait()

					if got := c.State(); got != wantState {
						t.Errorf("State() before completion notification = %v, want %v", got, wantState)
					}
					if got := c.Mailbox(); !reflect.DeepEqual(got, wantMailbox) {
						t.Errorf("Mailbox() before completion notification = %#v, want %#v", got, wantMailbox)
					}
					wantEnabled := fail || tt.name != "unauthenticate"
					c.mutex.Lock()
					gotEnabled := c.enabled.Has(imap.CapUTF8Accept)
					c.mutex.Unlock()
					if gotEnabled != wantEnabled {
						t.Errorf("enabled UTF8=ACCEPT = %v, want %v", gotEnabled, wantEnabled)
					}

					if err := cmd.base().wait(); err != cmdErr {
						t.Errorf("wait() = %v, want %v", err, cmdErr)
					}
					synctest.Wait()
					if _, err := contReq.Wait(); err == nil || (cmdErr != nil && err != cmdErr) {
						t.Errorf("continuation Wait() = %v, want cancellation with %v", err, cmdErr)
					}
				})
			})
		}
	}
}

func TestFRYTaggedCompletionInvalidatesCapabilitiesBeforeResult(t *testing.T) {
	tests := []struct {
		name           string
		newCmd         func() command
		status         string
		response       string
		wantReset      bool
		wantCapability bool
		wantStartTLS   bool
	}{
		{"authenticate", func() command { return &authenticateCommand{} }, "OK", " completed\r\n", true, false, false},
		{"login", func() command { return &loginCommand{} }, "OK", " completed\r\n", true, false, false},
		{"unauthenticate", func() command { return &unauthenticateCommand{} }, "OK", " completed\r\n", true, false, false},
		{"starttls", func() command { return &startTLSCommand{} }, "OK", " completed\r\n", true, false, true},
		{"login/rejected", func() command { return &loginCommand{} }, "NO", " rejected\r\n", false, false, false},
		{"login/capability-code", func() command { return &loginCommand{} }, "OK", " [CAPABILITY IMAP4rev2] completed\r\n", false, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				conn, peer := net.Pipe()
				defer conn.Close()
				defer peer.Close()

				cmd := tt.newCmd()
				cmd.base().tag = "T1"
				cmd.base().done = make(chan error)
				oldCaps := imap.CapSet{imap.CapStartTLS: {}}
				c := &Client{
					conn:        conn,
					bw:          bufio.NewWriter(io.Discard),
					dec:         imapwire.NewDecoder(bufio.NewReader(strings.NewReader(tt.response)), imapwire.ConnSideClient),
					caps:        oldCaps,
					enabled:     make(imap.CapSet),
					pendingCmds: []command{cmd},
				}

				type taggedResult struct {
					startTLS *startTLSCommand
					err      error
				}
				resultCh := make(chan taggedResult, 1)
				go func() {
					startTLS, err := c.readResponseTagged("T1", tt.status)
					resultCh <- taggedResult{startTLS, err}
				}()
				synctest.Wait() // the decoder is blocked publishing cmd.done

				c.mutex.Lock()
				gotCaps, pendingCapCh := c.caps, c.pendingCapCh
				c.mutex.Unlock()
				if tt.wantReset {
					if gotCaps != nil || pendingCapCh == nil {
						t.Errorf("before result: caps = %v, refresh pending = %v; want invalidated caps and pending refresh", gotCaps, pendingCapCh != nil)
					}
				} else if tt.wantCapability {
					if !gotCaps.Has(imap.CapIMAP4rev2) || gotCaps.Has(imap.CapStartTLS) || pendingCapCh != nil {
						t.Errorf("before result: caps = %v, refresh pending = %v; want response-code capabilities", gotCaps, pendingCapCh != nil)
					}
				} else if !reflect.DeepEqual(gotCaps, oldCaps) || pendingCapCh != nil {
					t.Errorf("before rejected result: caps = %v, refresh pending = %v; want old capabilities", gotCaps, pendingCapCh != nil)
				}

				cmdErr := cmd.base().wait()
				if (cmdErr == nil) != (tt.status == "OK") {
					t.Errorf("command wait error = %v, status = %s", cmdErr, tt.status)
				}
				synctest.Wait()

				result := <-resultCh
				if result.err != nil || (result.startTLS != nil) != tt.wantStartTLS {
					t.Errorf("tagged result = (%v, %v), want STARTTLS = %v", result.startTLS, result.err, tt.wantStartTLS)
				}
				if tt.wantReset {
					capCmd := findPendingCmdByType[*CapabilityCommand](c)
					if capCmd == nil {
						t.Error("capability refresh was not queued")
					} else {
						c.completeCommand(capCmd, nil)
						synctest.Wait()
					}
				}
			})
		})
	}
}

func TestFRYStartTLSRefreshWaitsForHandshakeLock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		conn, peer := net.Pipe()
		defer conn.Close()
		defer peer.Close()

		cmd := &startTLSCommand{commandBase: commandBase{tag: "T1", done: make(chan error, 1)}}
		c := &Client{
			conn:        conn,
			bw:          bufio.NewWriter(io.Discard),
			dec:         imapwire.NewDecoder(bufio.NewReader(strings.NewReader(" completed\r\n")), imapwire.ConnSideClient),
			caps:        imap.CapSet{imap.CapStartTLS: {}},
			pendingCmds: []command{cmd},
		}

		// startTLS holds encMutex until its TLS handshake returns. The
		// invalidation may queue a refresh, but it must not send cleartext.
		c.encMutex.Lock()
		startTLS, err := c.readResponseTagged("T1", "OK")
		if err != nil || startTLS != cmd {
			t.Errorf("tagged STARTTLS result = (%v, %v), want command", startTLS, err)
		}
		if err := cmd.wait(); err != nil {
			t.Errorf("STARTTLS wait = %v", err)
		}
		c.mutex.Lock()
		gotCaps, pendingCapCh := c.caps, c.pendingCapCh
		c.mutex.Unlock()
		if gotCaps != nil || pendingCapCh == nil {
			t.Errorf("caps = %v, refresh pending = %v; want invalidated caps and pending refresh", gotCaps, pendingCapCh != nil)
		}
		if capCmd := findPendingCmdByType[*CapabilityCommand](c); capCmd != nil {
			t.Error("CAPABILITY sent while STARTTLS handshake lock is held")
		}
		c.encMutex.Unlock()
		synctest.Wait()
		capCmd := findPendingCmdByType[*CapabilityCommand](c)
		if capCmd == nil {
			t.Error("CAPABILITY was not sent after STARTTLS handshake lock released")
		} else {
			c.completeCommand(capCmd, nil)
			synctest.Wait()
		}
	})
}

func TestFRYCommandStreamsCloseBeforeResult(t *testing.T) {
	tests := []struct {
		name   string
		newCmd func() command
		check  func(*testing.T, command)
	}{
		{"list", func() command {
			return &ListCommand{mailboxes: make(chan *imap.ListData, 1), pendingData: &imap.ListData{Mailbox: "INBOX"}}
		}, func(t *testing.T, cmd command) {
			stream := cmd.(*ListCommand).mailboxes
			select {
			case data, ok := <-stream:
				if !ok || data.Mailbox != "INBOX" {
					t.Errorf("LIST pending data = (%v, %v), want INBOX", data, ok)
				}
			default:
				t.Error("LIST pending data unavailable before result")
			}
			select {
			case _, ok := <-stream:
				if ok {
					t.Error("LIST stream still has data after pending result")
				}
			default:
				t.Error("LIST stream open before result")
			}
		}},
		{"fetch", func() command { return &FetchCommand{msgs: make(chan *FetchMessageData)} }, func(t *testing.T, cmd command) {
			select {
			case _, ok := <-cmd.(*FetchCommand).msgs:
				if ok {
					t.Error("FETCH stream yielded unexpected message")
				}
			default:
				t.Error("FETCH stream open before result")
			}
		}},
		{"expunge", func() command { return &ExpungeCommand{seqNums: make(chan uint32)} }, func(t *testing.T, cmd command) {
			select {
			case _, ok := <-cmd.(*ExpungeCommand).seqNums:
				if ok {
					t.Error("EXPUNGE stream yielded unexpected sequence number")
				}
			default:
				t.Error("EXPUNGE stream open before result")
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := &Client{}
				cmd := tt.newCmd()
				cmd.base().done = make(chan error)
				go c.completeCommand(cmd, nil)
				synctest.Wait()
				tt.check(t, cmd)
				if err := cmd.base().wait(); err != nil {
					t.Errorf("wait() = %v", err)
				}
				synctest.Wait()
			})
		})
	}
}

func TestFRYListCompletionDrainsFullStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := &Client{}
		cmd := &ListCommand{
			mailboxes:   make(chan *imap.ListData, 1),
			pendingData: &imap.ListData{Mailbox: "second"},
		}
		cmd.mailboxes <- &imap.ListData{Mailbox: "first"}
		cmd.done = make(chan error)
		go c.completeCommand(cmd, nil)
		mailboxes, err := cmd.Collect()
		if err != nil {
			t.Fatalf("Collect() = %v", err)
		}
		if len(mailboxes) != 2 || mailboxes[0].Mailbox != "first" || mailboxes[1].Mailbox != "second" {
			t.Errorf("Collect() = %v, want first and second", mailboxes)
		}
	})
}

type fryFailWriter struct{ err error }

func (w fryFailWriter) Write([]byte) (int, error) { return 0, w.err }

func TestFRYRefreshWriteFailureKeepsLogout(t *testing.T) {
	tests := []struct {
		name    string
		newCmd  func() command
		initial imap.ConnState
	}{
		{"authenticate", func() command { return &authenticateCommand{} }, imap.ConnStateNotAuthenticated},
		{"login", func() command { return &loginCommand{} }, imap.ConnStateNotAuthenticated},
		{"unauthenticate", func() command { return &unauthenticateCommand{} }, imap.ConnStateAuthenticated},
		{"select", func() command { return &SelectCommand{mailbox: "INBOX"} }, imap.ConnStateAuthenticated},
		{"unselect", func() command { return &unselectCommand{} }, imap.ConnStateSelected},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				conn, peer := net.Pipe()
				defer conn.Close()
				defer peer.Close()

				writeErr := errors.New("CAPABILITY write failed")
				cmd := tt.newCmd()
				cmd.base().tag = "T1"
				cmd.base().done = make(chan error, 1)
				c := &Client{
					conn:        conn,
					bw:          bufio.NewWriter(fryFailWriter{err: writeErr}),
					state:       tt.initial,
					caps:        imap.CapSet{imap.CapStartTLS: {}},
					enabled:     make(imap.CapSet),
					pendingCmds: []command{cmd},
				}

				// A tagged response is removed from pendingCmds before its
				// completion. Force this refresh writer to close the connection
				// in that gap. For SELECT/UNSELECT, this models a concurrent
				// writer's failure while their tagged response completes.
				if got := c.deletePendingCmdByTag("T1"); got != cmd {
					t.Fatalf("deletePendingCmdByTag() = %v, want command", got)
				}
				c.setCaps(nil)
				c.mutex.Lock()
				capCh := c.pendingCapCh
				c.mutex.Unlock()
				<-capCh // the writer failed and closeWithError completed
				if got := c.State(); got != imap.ConnStateLogout {
					t.Fatalf("State() after refresh write failure = %v, want logout", got)
				}

				c.completeCommand(cmd, nil)
				if err := cmd.base().wait(); err != nil {
					t.Errorf("original tagged command wait = %v, want nil", err)
				}
				if got := c.State(); got != imap.ConnStateLogout {
					t.Errorf("State() after tagged completion = %v, want logout", got)
				}
			})
		})
	}
}
