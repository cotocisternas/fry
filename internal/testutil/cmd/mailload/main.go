// Command mailload measures one bounded synthetic numeric-loopback mailbox load.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"os"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

type config struct {
	smtp, imap, user, password string
	count, size, concurrency   int
	timeout                    time.Duration
}
type outcome struct {
	index      int
	elapsed    time.Duration
	acceptedAt time.Time
	err        error
}
type report struct {
	Scope             string             `json:"scope"`
	StartedAt         time.Time          `json:"started_at"`
	Run               string             `json:"run"`
	Count             int                `json:"requested"`
	MessageBytes      int                `json:"message_bytes"`
	Concurrency       int                `json:"concurrency"`
	Accepted          int                `json:"accepted"`
	Deferred          int                `json:"deferred"`
	Failed            int                `json:"failed"`
	ElapsedSeconds    float64            `json:"smtp_elapsed_seconds"`
	AcceptedPerSecond float64            `json:"accepted_per_second"`
	AcceptLatency     map[string]float64 `json:"accept_latency_ms"`
	Publication       map[string]float64 `json:"publication"`
	IMAP              map[string]float64 `json:"imap"`
	VerifiedHeaders   int                `json:"verified_headers"`
	VerifiedSamples   int                `json:"verified_fetch_samples"`
	AllocatedBytes    uint64             `json:"client_allocated_bytes"`
	Metadata          map[string]any     `json:"metadata"`
	Errors            []string           `json:"errors,omitempty"`
}

func endpoint(value string) error {
	host, port, err := net.SplitHostPort(value)
	ip := net.ParseIP(host)
	number, e := strconv.Atoi(port)
	if err != nil || ip == nil || !ip.IsLoopback() || e != nil || number < 1 || number > 65535 {
		return fmt.Errorf("only numeric loopback endpoints with explicit ports are allowed")
	}
	return nil
}

func validate(c config) error {
	if err := endpoint(c.smtp); err != nil {
		return err
	}
	if err := endpoint(c.imap); err != nil {
		return err
	}
	if !validWorkload(c) || c.user == "" || c.password == "" {
		return fmt.Errorf(
			"invalid bounded workload: count1..50000, bytes1024..1048576, concurrency1..128, " +
				"total<=1GiB, timeout1s..2m, credentials required",
		)
	}
	return nil
}

func message(run string, index, size int) []byte {
	prefix := fmt.Sprintf(
		"From: synthetic@load.test\r\nTo: invoices@example.test\r\nDate: Tue, 13 Oct 2026 "+
			"12:00:00 +0000\r\nSubject: fry-load-%s\r\nMessage-ID: <%s-%d@load.test>\r\n"+
			"X-Fry-Load-Index: %d\r\nX-Fry-Load-SHA256: %s\r\n\r\n",
		run,
		run,
		index,
		index,
		strings.Repeat("0", 64),
	)
	body := []byte(strings.Repeat("x", size-len(prefix)-2) + "\r\n")
	// Make the body itself distinct, while keeping SMTP lines below 998 bytes.
	marker := fmt.Sprintf("synthetic-index=%08d;", index)
	copy(body, marker)
	for offset := 800; offset < len(body)-2; offset += 800 {
		body[offset] = '\r'
		body[offset+1] = '\n'
	}
	digest := sha256.Sum256(body)
	prefix = strings.Replace(prefix, strings.Repeat("0", 64), hex.EncodeToString(digest[:]), 1)
	return append([]byte(prefix), body...)
}

func dialSMTP(ctx context.Context, address string) (*smtp.Client, net.Conn, error) {
	connection, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, nil, err
	}
	deadline := time.Now().Add(10 * time.Second)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	if err = connection.SetDeadline(deadline); err != nil {
		return nil, nil, errors.Join(err, connection.Close())
	}
	stop := context.AfterFunc(ctx, func() { abortTransport(connection) })
	defer stop()
	client, err := smtp.NewClient(connection, "localhost")
	if err != nil {
		return nil, nil, errors.Join(err, connection.Close())
	}
	return client, connection, nil
}

func deliver(client *smtp.Client, to string, body []byte) error {
	if err := client.Mail("synthetic@load.test"); err != nil {
		return err
	}
	if err := client.Rcpt(to); err != nil {
		return err
	}
	writer, err := client.Data()
	if err != nil {
		return err
	}
	if _, err = writer.Write(body); err != nil {
		// Closing the DATA writer would submit a partial message. Abort the
		// underlying SMTP transport without emitting the DATA terminator.
		return errors.Join(err, client.Close())
	}
	if err := writer.Close(); err != nil {
		return errors.Join(err, client.Close())
	}
	return nil
}

func durations(values []float64) map[string]float64 {
	sort.Float64s(values)
	result := map[string]float64{}
	if len(values) == 0 {
		return result
	}
	for _, p := range []int{50, 95, 99} {
		i := (len(values)*p+99)/100 - 1
		result[fmt.Sprintf("p%d", p)] = values[i]
	}
	return result
}

func smtpLoad(ctx context.Context, c config, run string, r *report) []bool {
	results := make(chan outcome, c.concurrency)
	var next atomic.Int64
	var workers sync.WaitGroup
	start := time.Now()
	for range c.concurrency {
		workers.Go(func() {
			runSMTPWorker(ctx, c, run, &next, results)
		})
	}
	go func() { workers.Wait(); close(results) }()
	accepted := make([]bool, c.count)
	var samples []float64
	var first, last time.Time
	for result := range results {
		if result.err == nil {
			r.Accepted++
			accepted[result.index] = true
			samples = append(samples, float64(result.elapsed)/float64(time.Millisecond))
			if first.IsZero() || result.acceptedAt.Before(first) {
				first = result.acceptedAt
			}
			if result.acceptedAt.After(last) {
				last = result.acceptedAt
			}
		} else {
			recordSMTPFailure(r, result)
		}
	}
	r.ElapsedSeconds = time.Since(start).Seconds()
	r.AcceptedPerSecond = float64(r.Accepted) / r.ElapsedSeconds
	r.AcceptLatency = durations(samples)
	if !first.IsZero() {
		r.Publication["first_accept_unix_seconds"] = float64(first.UnixNano()) / 1e9
		r.Publication["last_accept_unix_seconds"] = float64(last.UnixNano()) / 1e9
	}
	return accepted
}

func runSMTPWorker(ctx context.Context, c config, run string, next *atomic.Int64, results chan<- outcome) {
	var client *smtp.Client
	var connection net.Conn
	defer func() {
		if client != nil {
			// All message outcomes are already final; teardown cannot
			// change an SMTP acknowledgement or add another outcome.
			abortTransport(connection)
		}
	}()
	for {
		index := int(next.Add(1)) - 1
		if index >= c.count {
			return
		}
		begin := time.Now()
		var err error
		client, connection, err = deliverLoadMessage(ctx, c, run, index, client, connection)

		end := time.Now()
		if err != nil && client != nil {
			err = errors.Join(err, client.Close())
			client = nil
		}
		results <- outcome{index: index, elapsed: end.Sub(begin), acceptedAt: end, err: err}
	}
}

func deliverLoadMessage(
	ctx context.Context,
	c config,
	run string,
	index int,
	client *smtp.Client,
	connection net.Conn,
) (*smtp.Client, net.Conn, error) {
	var err error
	if err := ctx.Err(); err != nil {
		return client, connection, err
	}
	if client == nil {
		client, connection, err = dialSMTP(ctx, c.smtp)
		if err != nil {
			return client, connection, err
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	if err := connection.SetDeadline(deadline); err != nil {
		return client, connection, err
	}
	stop := context.AfterFunc(ctx, func() { abortTransport(connection) })
	defer stop()
	to := []string{"invoices@example.test", "billing@example.test"}[index%2]
	return client, connection, deliver(client, to, message(run, index, c.size))
}

func verify(ctx context.Context, c config, run string, accepted []bool, r *report) (retErr error) {
	connection, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", c.imap)
	if err != nil {
		return err
	}
	// Before the IMAP client takes ownership, failures still close the transport.
	defer func() {
		if connection != nil {
			retErr = errors.Join(retErr, connection.Close())
		}
	}()
	end, _ := ctx.Deadline()
	if err = connection.SetDeadline(end); err != nil {
		return err
	}
	currentConnection := connection
	stop := context.AfterFunc(ctx, func() { abortTransport(currentConnection) })
	defer stop()
	client := imapclient.New(connection, nil)
	connection = nil // client now owns the transport
	defer func() { retErr = errors.Join(retErr, client.Close()) }()
	if err = client.Login(c.user, c.password).Wait(); err != nil {
		return err
	}
	return verifyMailbox(ctx, client, c, run, accepted, r)
}

func verifyMailbox(
	ctx context.Context,
	client *imapclient.Client,
	c config,
	run string,
	accepted []bool,
	r *report,
) error {
	var err error
	begin := time.Now()
	if err := waitMailboxVisible(ctx, client, r.Accepted); err != nil {
		return err
	}
	r.Publication["wait_after_smtp_ms"] = float64(time.Since(begin)) / float64(time.Millisecond)
	r.Publication["all_visible_unix_seconds"] = float64(time.Now().UnixNano()) / 1e9
	if last, ok := r.Publication["last_accept_unix_seconds"]; ok {
		r.Publication["last_accept_to_all_visible_ms"] = (r.Publication["all_visible_unix_seconds"] - last) * 1000
	}
	begin = time.Now()
	selected, err := client.Select("INBOX", nil).Wait()
	if err != nil {
		return err
	}
	r.IMAP["select_ms"] = float64(time.Since(begin)) / float64(time.Millisecond)
	r.IMAP["messages"] = float64(selected.NumMessages)
	begin = time.Now()
	search, err := client.UIDSearch(loadSearchCriteria(run), nil).
		Wait()
	if err != nil {
		return err
	}
	r.IMAP["uid_search_header_date_unseen_ms"] = float64(time.Since(begin)) / float64(time.Millisecond)
	r.IMAP["uid_search_header_date_unseen_cold_ms"] = r.IMAP["uid_search_header_date_unseen_ms"]
	begin = time.Now()
	warm, err := client.UIDSearch(loadSearchCriteria(run), nil).
		Wait()
	if err != nil {
		return err
	}
	r.IMAP["uid_search_header_date_unseen_warm_ms"] = float64(time.Since(begin)) / float64(time.Millisecond)
	if !slices.Equal(search.AllUIDs(), warm.AllUIDs()) {
		return fmt.Errorf("identical warm search changed UID results")
	}
	uids := search.AllUIDs()
	if len(uids) != r.Accepted {
		return fmt.Errorf("header/date/unseen search returned %d, expected %d", len(uids), r.Accepted)
	}
	if err := verifyHeaders(client, c, run, accepted, uids, r); err != nil {
		return err
	}
	if err := verifySamples(client, c, run, accepted, uids, r); err != nil {
		return err
	}
	return verifySeenFlag(client, uids, r)
}

func verifySeenFlag(client *imapclient.Client, uids []imap.UID, r *report) error {
	var err error
	var begin time.Time
	if len(uids) > 0 {
		if _, err = client.Store(imap.UIDSetNum(uids[0]), &imap.StoreFlags{
			Op:    imap.StoreFlagsAdd,
			Flags: []imap.Flag{imap.FlagSeen},
		}, nil).
			Collect(); err != nil {
			return err
		}
		begin = time.Now()
		seen, err := client.UIDSearch(&imap.SearchCriteria{Flag: []imap.Flag{imap.FlagSeen}}, nil).Wait()
		if err != nil {
			return err
		}
		r.IMAP["uid_search_seen_ms"] = float64(time.Since(begin)) / float64(time.Millisecond)
		if got := seen.AllUIDs(); len(got) != 1 || got[0] != uids[0] {
			return fmt.Errorf("flag search mismatch")
		}
	}
	return nil
}

func verifySamples(client *imapclient.Client, c config, run string, accepted []bool, uids []imap.UID, r *report) error {
	var begin time.Time
	for sample := range min(len(uids), 8) {
		uid := uids[sample*(len(uids)-1)/max(1, min(len(uids), 8)-1)]
		section := &imap.FetchItemBodySection{Peek: true}
		begin = time.Now()
		fetched, err := client.Fetch(imap.UIDSetNum(uid), &imap.FetchOptions{
			UID:          true,
			Flags:        true,
			RFC822Size:   true,
			InternalDate: true,
			BodySection:  []*imap.FetchItemBodySection{section},
		}).
			Collect()
		if err != nil {
			return err
		}
		if len(fetched) != 1 {
			return fmt.Errorf("sample UID%d missing", uid)
		}
		raw := fetched[0].FindBodySection(section)
		if err := verifySampleBody(fetched[0], raw, uid, c, run, accepted); err != nil {
			return err
		}
		partial := &imap.FetchItemBodySection{Peek: true, Partial: &imap.SectionPartial{Offset: 9, Size: 97}}
		part, err := client.Fetch(imap.UIDSetNum(uid), &imap.FetchOptions{
			BodySection: []*imap.FetchItemBodySection{partial},
		}).
			Collect()
		if err != nil {
			return err
		}
		if len(part) != 1 || !bytes.Equal(part[0].FindBodySection(partial), raw[9:106]) {
			return fmt.Errorf("partial FETCH mismatch")
		}
		r.IMAP["sample_fetch_total_ms"] += float64(time.Since(begin)) / float64(time.Millisecond)
		r.VerifiedSamples++
	}
	return nil
}

func verifySampleBody(
	fetched *imapclient.FetchMessageBuffer,
	raw []byte,
	uid imap.UID,
	c config,
	run string,
	accepted []bool,
) error {
	parsed, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	index, err := acceptedMessageIndex(parsed, accepted)
	if err != nil {
		return fmt.Errorf("unexpected load index")
	}
	body, err := io.ReadAll(parsed.Body)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(body)
	if parsed.Header.Get("X-Fry-Load-SHA256") != hex.EncodeToString(digest[:]) ||
		!bytes.Equal(raw, message(run, index, c.size)) ||
		!validFetchedMetadata(fetched, c.size) {
		return fmt.Errorf("sample UID%d content/date/flags/size mismatch", uid)
	}
	return nil
}

func verifyHeaders(client *imapclient.Client, c config, run string, accepted []bool, uids []imap.UID, r *report) error {
	var begin time.Time
	if len(uids) > 0 {
		headerSection := &imap.FetchItemBodySection{
			Peek:         true,
			Specifier:    imap.PartSpecifierHeader,
			HeaderFields: []string{"Subject", "Date", "X-Fry-Load-Index", "X-Fry-Load-SHA256"},
		}
		begin = time.Now()
		command := client.Fetch(
			imap.UIDSetNum(uids...),
			&imap.FetchOptions{
				UID:          true,
				Flags:        true,
				RFC822Size:   true,
				InternalDate: true,
				BodySection:  []*imap.FetchItemBodySection{headerSection},
			},
		)
		seen := make([]bool, c.count)
		for item := command.Next(); item != nil; item = command.Next() {
			fetched, err := item.Collect()
			if err != nil {
				return errors.Join(err, command.Close())
			}
			index, err := verifyHeader(fetched, headerSection, c, run, accepted, seen)
			if err != nil {
				return errors.Join(err, command.Close())
			}

			seen[index] = true
			r.VerifiedHeaders++
		}
		if err := command.Close(); err != nil {
			return err
		}
		r.IMAP["all_headers_metadata_fetch_ms"] = float64(time.Since(begin)) / float64(time.Millisecond)
		if r.VerifiedHeaders != r.Accepted {
			return fmt.Errorf("header count mismatch")
		}
	}
	return nil
}

func verifyHeader(
	fetched *imapclient.FetchMessageBuffer,
	headerSection *imap.FetchItemBodySection,
	c config,
	run string,
	accepted, seen []bool,
) (int, error) {
	parsed, err := mail.ReadMessage(bytes.NewReader(fetched.FindBodySection(headerSection)))
	if err != nil {
		return 0, err
	}
	index, err := acceptedMessageIndex(parsed, accepted)
	if err != nil || seen[index] {
		return 0, fmt.Errorf("missing, unexpected, or duplicate message index in header scan")
	}
	expected, err := mail.ReadMessage(bytes.NewReader(message(run, index, c.size)))
	if err != nil {
		return 0, err
	}
	for _, key := range []string{"Subject", "Date", "X-Fry-Load-SHA256"} {
		if parsed.Header.Get(key) != expected.Header.Get(key) {
			return 0, fmt.Errorf("header %s mismatch at index%d", key, index)
		}
	}
	if !validFetchedMetadata(fetched, c.size) {
		return 0, fmt.Errorf("metadata mismatch at index%d", index)
	}
	return index, nil
}

func acceptedMessageIndex(parsed *mail.Message, accepted []bool) (int, error) {
	index, err := strconv.Atoi(parsed.Header.Get("X-Fry-Load-Index"))
	if err != nil || index < 0 || index >= len(accepted) || !accepted[index] {
		return 0, fmt.Errorf("unexpected load index")
	}
	return index, nil
}

func validFetchedMetadata(fetched *imapclient.FetchMessageBuffer, size int) bool {
	return fetched.RFC822Size == int64(size) && !fetched.InternalDate.IsZero() && len(fetched.Flags) == 0
}

func waitMailboxVisible(ctx context.Context, client *imapclient.Client, accepted int) error {
	var err error
	var selected *imap.SelectData
	for {
		selected, err = client.Select("INBOX", nil).Wait()
		if err != nil {
			return err
		}
		if int(selected.NumMessages) == accepted {
			break
		}
		if int(selected.NumMessages) > accepted {
			return fmt.Errorf("fixture inbox contains unexpected messages: %d", selected.NumMessages)
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
	return nil
}

func metadata() map[string]any {
	m := map[string]any{
		"go":            runtime.Version(),
		"os":            runtime.GOOS,
		"arch":          runtime.GOARCH,
		"logical_cpus":  runtime.NumCPU(),
		"gomaxprocs":    runtime.GOMAXPROCS(0),
		"process_scope": "client allocation/RSS only; server and PostgreSQL are separate processes",
	}
	for _, file := range []string{"/proc/cpuinfo", "/proc/meminfo", "/proc/self/status"} {
		var data []byte
		var err error
		switch file {
		case "/proc/cpuinfo":
			data, err = os.ReadFile("/proc/cpuinfo")
		case "/proc/meminfo":
			data, err = os.ReadFile("/proc/meminfo")
		case "/proc/self/status":
			data, err = os.ReadFile("/proc/self/status")
		}
		if err != nil {
			continue
		}
		parseProcessMetadata(m, file, data)
	}
	return m
}

func run() error {
	c := config{}
	flag.StringVar(&c.smtp, "smtp", "127.0.0.1:2525", "synthetic numeric loopback SMTP endpoint")
	flag.StringVar(&c.imap, "imap", "127.0.0.1:1143", "synthetic numeric loopback IMAP endpoint")
	flag.StringVar(&c.user, "user", "invoices", "synthetic fixture username")
	flag.IntVar(&c.count, "count", 1000, "message count, max50000")
	flag.IntVar(&c.size, "bytes", 64<<10, "raw message bytes, max1MiB")
	flag.IntVar(&c.concurrency, "concurrency", 16, "sequential SMTP workers, max128")
	flag.DurationVar(&c.timeout, "timeout", 115*time.Second, "whole run deadline, max2m")
	flag.Parse()
	c.password = os.Getenv("FRY_LOAD_IMAP_PASSWORD")
	if err := validate(c); err != nil {
		return err
	}
	token := make([]byte, 8)
	if _, err := rand.Read(token); err != nil {
		return err
	}
	run := hex.EncodeToString(token)
	r := report{
		Scope:        "synthetic local concentrated-inbox baseline; not a production capacity benchmark",
		StartedAt:    time.Now().UTC(),
		Run:          run,
		Count:        c.count,
		MessageBytes: c.size,
		Concurrency:  c.concurrency,
		Publication:  map[string]float64{},
		IMAP:         map[string]float64{},
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	accepted := smtpLoad(ctx, c, run, &r)
	err := verify(ctx, c, run, accepted, &r)
	if err != nil {
		r.Errors = append(r.Errors, "IMAP verification failed")
	}
	runtime.ReadMemStats(&after)
	r.AllocatedBytes = after.TotalAlloc - before.TotalAlloc
	r.Metadata = metadata()
	encodeErr := json.NewEncoder(os.Stdout).Encode(r)
	if encodeErr != nil {
		return encodeErr
	}
	if err != nil {
		return err
	}
	if r.Deferred+r.Failed != 0 {
		return fmt.Errorf("workload had %d deferred and %d failed messages", r.Deferred, r.Failed)
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		if _, writeErr := fmt.Fprintln(os.Stderr, "synthetic mail load failed"); writeErr != nil {
			os.Exit(1)
		}
		os.Exit(1)
	}
}

// abortTransport releases a canceled or completed transport. A close failure
// cannot restore a canceled exchange or change an acknowledged message outcome.
func abortTransport(connection net.Conn) {
	if err := connection.Close(); err != nil {
		return
	}
}

func validWorkload(c config) bool {
	return c.count >= 1 && c.count <= 50000 && c.size >= 1024 && c.size <= 1<<20 && c.concurrency >= 1 &&
		c.concurrency <= 128 &&
		int64(c.count)*int64(c.size) <= 1<<30 &&
		c.timeout >= time.Second &&
		c.timeout <= 2*time.Minute
}

func parseProcessMetadata(m map[string]any, file string, data []byte) {
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.HasPrefix(line, "model name") || strings.HasPrefix(line, "MemTotal:") ||
			strings.HasPrefix(line, "VmHWM:") {
			key, value, ok := strings.Cut(line, ":")
			if ok {
				m[strings.TrimSpace(key)] = strings.TrimSpace(value)
			}
			if file == "/proc/cpuinfo" {
				break
			}
		}
	}
}

func recordSMTPFailure(r *report, result outcome) {
	var response *textproto.Error
	if errors.As(result.err, &response) && response.Code >= 400 && response.Code < 500 {
		r.Deferred++
	} else {
		r.Failed++
	}
	if len(r.Errors) < 8 {
		r.Errors = append(r.Errors, fmt.Sprintf("index %d: SMTP delivery or transport failed", result.index))
	}
}

func loadSearchCriteria(run string) *imap.SearchCriteria {
	return &imap.SearchCriteria{
		Header:     []imap.SearchCriteriaHeaderField{{Key: "Subject", Value: "fry-load-" + run}},
		SentSince:  time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC),
		SentBefore: time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC),
		NotFlag:    []imap.Flag{imap.FlagSeen},
	}
}
