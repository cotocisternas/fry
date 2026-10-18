//go:build linux

package imapserver

import (
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
)

func TestEnvelopeGroupsEncodedWordsAndMissingFields(t *testing.T) {
	raw := "Date: Fri, 16 Oct 2026 12:30:00 -0300\r\n" +
		"From: =?iso-8859-1?Q?Jos=E9?= <jose@example.test>\r\n" +
		"To: \"Accounts: Brazil\": \"Doe, Jane\" <jane@example.test>, " +
		"john@[IPv6:2001:db8::1];, Undisclosed:;\r\nCc: other@example.test\r\n" +
		"Cc: second@example.test\r\nSubject: =?utf-8?Q?Invoice_=E2=82=AC?=\r\n" +
		"Message-ID: <invoice-42@example.test>\r\n" +
		"In-Reply-To: <first@example.test> <second@example.test>\r\n\r\n"
	result, err := envelope([]byte(raw))
	requireTestCondition(t, err == nil, err)
	requireTestConditionf(t, result.Subject == "Invoice €", "bad envelope: %+v", result)
	requireTestConditionf(t, result.From[0].Name == "José", "bad envelope: %+v", result)
	requireTestConditionf(t, len(result.To) == 6, "bad envelope: %+v", result)
	requireTestConditionf(t, result.To[0].Mailbox == "Accounts: Brazil", "bad envelope: %+v", result)
	requireTestConditionf(t, result.To[0].IsGroupStart(), "bad envelope: %+v", result)
	requireTestConditionf(t, result.To[1].Name == "Doe, Jane", "bad envelope: %+v", result)
	requireTestConditionf(t, result.To[2].Host == "[IPv6:2001:db8::1]", "bad envelope: %+v", result)
	requireTestConditionf(t, result.To[3].IsGroupEnd(), "bad envelope: %+v", result)
	requireTestConditionf(t, result.To[4].Mailbox == "Undisclosed", "bad envelope: %+v", result)
	requireTestConditionf(t, result.To[5].IsGroupEnd(), "bad envelope: %+v", result)
	requireTestConditionf(t, len(result.Cc) == 2, "bad envelope: %+v", result)
	requireTestConditionf(t, result.MessageID == "invoice-42@example.test", "bad envelope: %+v", result)
	requireTestConditionf(t, len(result.InReplyTo) == 2, "bad envelope: %+v", result)
	empty, err := envelope([]byte("Subject: Only a subject\r\nDate: invalid\r\n\r\n"))
	requireTestCondition(t, err == nil, empty, err)
	requireTestCondition(t, empty.Date.IsZero(), empty, err)
	requireTestCondition(t, empty.From == nil, empty, err)
	requireTestCondition(t, empty.Subject == "Only a subject", empty, err)
	for _, bad := range []string{
		"Group: name@example.test",
		"Group: Nested: member@example.test;;",
		strings.Repeat("(", 17) + "comment" + strings.Repeat(")", 17) +
			" a@example.test",
	} {
		{
			_, err := envelopeAddresses(bad)
			requireTestConditionf(t, err != nil, "invalid address accepted: %s", bad)
		}
	}
}

func TestIMAPEnvelopeFetchFallbackAndRawPreservation(t *testing.T) {
	f := setup(t)
	raw := "Date: Fri, 16 Oct 2026 12:30:00 -0300\r\n" +
		"From: =?utf-8?Q?Jos=C3=A9?= <sender@example.test>\r\n" +
		"To: Team: member@example.test;, Empty:;\r\n" +
		"Subject: =?utf-8?Q?Fatura_n=C3=BAmero_42?=\r\nMessage-ID: <42@example.test>\r\n" +
		"\r\noriginal bytes\r\n"
	f.add(t, raw)
	c := f.client(t)
	{
		_, err := c.Select("INBOX", nil).Wait()
		requireTestCondition(t, err == nil, err)
	}
	section := &imap.FetchItemBodySection{Peek: true}
	data, err := c.Fetch(
		imap.UIDSetNum(1),
		&imap.FetchOptions{
			Envelope:    true,
			BodySection: []*imap.FetchItemBodySection{section},
		},
	).
		Collect()
	requireTestCondition(t, err == nil, data, err)
	requireTestCondition(t, len(data) == 1, data, err)
	e := data[0].Envelope
	requireTestConditionf(t, e != nil, "bad fetched envelope: %+v", e)
	requireTestConditionf(t, e.Subject == "Fatura número 42", "bad fetched envelope: %+v", e)
	requireTestConditionf(t, e.From[0].Name == "José", "bad fetched envelope: %+v", e)
	requireTestConditionf(t, len(e.Sender) == 1, "bad fetched envelope: %+v", e)
	requireTestConditionf(t, e.Sender[0].Addr() == e.From[0].Addr(), "bad fetched envelope: %+v", e)
	requireTestConditionf(t, len(e.ReplyTo) == 1, "bad fetched envelope: %+v", e)
	requireTestConditionf(t, len(e.To) == 5, "bad fetched envelope: %+v", e)
	requireTestConditionf(t, e.To[0].IsGroupStart(), "bad fetched envelope: %+v", e)
	requireTestConditionf(t, e.To[2].IsGroupEnd(), "bad fetched envelope: %+v", e)
	requireTestConditionf(t, e.To[3].IsGroupStart(), "bad fetched envelope: %+v", e)
	requireTestConditionf(t, e.To[4].IsGroupEnd(), "bad fetched envelope: %+v", e)
	requireTestConditionf(
		t,
		e.Date.UTC().Equal(time.Date(2026, 10, 16, 15, 30, 0, 0, time.UTC)),
		"bad fetched envelope: %+v",
		e,
	)
	requireTestCondition(t, string(data[0].FindBodySection(section)) == raw, "parsed envelope altered raw message")
}
