//go:build linux

package imapserver

import (
	"io"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/emersion/go-imap/v2"
)

func TestIMAPMIMEStructureAndOriginalPartSections(t *testing.T) {
	f := setup(t)
	partHeader := "Content-Type: application/pdf; name=invoice.pdf\r\n" +
		"Content-Disposition: attachment; filename*=utf-8''fatura-%E2%82%AC.pdf\r\n" +
		"Content-Transfer-Encoding: base64\r\nContent-ID: <document>\r\n\r\n"
	embeddedHeader := "From: source@example.test\r\nSubject: =?utf-8?Q?Nested_invoice?=\r\n" +
		"Content-Type: multipart/alternative; boundary=nested\r\n\r\n"
	embeddedBody := "--nested\r\n\r\nplain\r\n--nested\r\nContent-Type: text/html\r\n\r\n" +
		"<b>html</b>\r\n--nested--\r\n"
	attachment := "JVBERi0xLjQ=\r\nYWJj"
	raw := "Subject: invoice\r\nContent-Type: multipart/mixed; boundary=outer\r\n\r\n" +
		"preamble\r\n--outer\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nhello\r\n" +
		"--outer\r\n" +
		partHeader + attachment +
		"\r\n--outer\r\nContent-Type: message/rfc822\r\n\r\n" +
		embeddedHeader + embeddedBody +
		"\r\n--outer--\r\nepilogue"
	f.add(t, raw)
	c := f.client(t)
	{
		_, err := c.Select("INBOX", nil).Wait()
		requireTestCondition(t, err == nil, err)
	}
	tests := []mimeSectionExpectation{
		{&imap.FetchItemBodySection{}, raw},
		{&imap.FetchItemBodySection{Part: []int{1}}, "hello"},
		{&imap.FetchItemBodySection{Part: []int{2}}, attachment},
		{&imap.FetchItemBodySection{Part: []int{2}, Specifier: imap.PartSpecifierMIME}, partHeader},
		{
			&imap.FetchItemBodySection{Part: []int{2}, Partial: &imap.SectionPartial{Offset: 3, Size: 5}},
			attachment[3:8],
		},
		{&imap.FetchItemBodySection{Part: []int{2}, Partial: &imap.SectionPartial{Offset: 1000, Size: 50}}, ""},
		{&imap.FetchItemBodySection{Part: []int{3}}, embeddedHeader + embeddedBody},
		{&imap.FetchItemBodySection{Part: []int{3}, Specifier: imap.PartSpecifierHeader}, embeddedHeader},
		{
			&imap.FetchItemBodySection{
				Part:         []int{3},
				Specifier:    imap.PartSpecifierHeader,
				HeaderFields: []string{"Subject"},
			},
			"Subject: =?utf-8?Q?Nested_invoice?=\r\n\r\n",
		},
		{&imap.FetchItemBodySection{Part: []int{3}, Specifier: imap.PartSpecifierText}, embeddedBody},
		{&imap.FetchItemBodySection{Part: []int{3, 1}}, "plain"},
		{&imap.FetchItemBodySection{Part: []int{3, 2}}, "<b>html</b>"},
	}
	data := fetchMIMESections(t, c, tests)
	assertMIMEInvoiceStructure(t, data.BodyStructure, attachment, embeddedHeader+embeddedBody)
	// The unextended BODY response must also encode a valid tree.
	{
		_, err := c.Fetch(imap.UIDSetNum(1), &imap.FetchOptions{BodyStructure: &imap.FetchItemBodyStructure{}}).
			Collect()
		requireTestCondition(t, err == nil, err)
	}
}

func TestIMAPMalformedMIMEKeepsRawAccessible(t *testing.T) {
	f := setup(t)
	raw := "Content-Type: multipart/mixed; boundary=missing\r\n\r\n--missing\r\n\r\nunfinished"
	f.add(t, raw)
	c := f.client(t)
	{
		_, err := c.Select("INBOX", nil).Wait()
		requireTestCondition(t, err == nil, err)
	}
	{
		_, err := c.Fetch(
			imap.UIDSetNum(1),
			&imap.FetchOptions{BodyStructure: &imap.FetchItemBodyStructure{Extended: true}},
		).
			Collect()
		requireTestCondition(t, err != nil, "malformed MIME accepted as complete metadata")
	}
	item := &imap.FetchItemBodySection{Peek: true}
	data, err := c.Fetch(imap.UIDSetNum(1), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{item}}).
		Collect()
	requireTestCondition(t, err == nil, "raw mail unavailable", err)
	requireTestCondition(t, len(data) == 1, "raw mail unavailable", err)
	requireTestCondition(t, string(data[0].FindBodySection(item)) == raw, "raw mail unavailable", err)
}

func TestIMAPAbsentSectionIsNilAndEmptySectionIsLiteral(t *testing.T) {
	f := setup(t)
	f.add(t, "Content-Type: text/plain\r\n\r\n")
	c := f.client(t)
	{
		_, err := c.Select("INBOX", nil).Wait()
		requireTestCondition(t, err == nil, err)
	}
	command := c.Fetch(
		imap.UIDSetNum(1),
		&imap.FetchOptions{
			BodySection: []*imap.FetchItemBodySection{{Part: []int{1}, Peek: true}, {Part: []int{2}, Peek: true}},
		},
	)
	present, absent := false, false
	for message := command.Next(); message != nil; message = command.Next() {
		for item := message.Next(); item != nil; item = message.Next() {
			section, ok := item.(imapclient.FetchItemDataBodySection)
			if !ok {
				continue
			}
			switch section.Section.Part[0] {
			case 1:
				requireTestCondition(t, section.Literal != nil, "empty body became NIL")
				data, err := io.ReadAll(section.Literal)
				requireTestCondition(t, err == nil, data, err)
				requireTestCondition(t, len(data) == 0, data, err)
				present = true
			case 2:
				requireTestCondition(t, section.Literal == nil, "nonexistent body became a literal")
				absent = true
			}
		}
	}
	{
		err := command.Close()
		requireTestCondition(t, err == nil, "section response mismatch", present, absent, err)
		requireTestCondition(t, present, "section response mismatch", present, absent, err)
		requireTestCondition(t, absent, "section response mismatch", present, absent, err)
	}
}

func assertMIMEInvoiceStructure(t *testing.T, body imap.BodyStructure, attachment, embedded string) {
	t.Helper()
	structure, ok := body.(*imap.BodyStructureMultiPart)
	requireTestConditionf(t, ok, "unexpected body structure: %T", body)
	requireTestConditionf(t, len(structure.Children) == 3, "structure %+v", structure)
	requireTestConditionf(t, structure.Extended.Params["boundary"] == "outer", "structure %+v", structure)
	pdf, ok := structure.Children[1].(*imap.BodyStructureSinglePart)
	requireTestConditionf(t, ok, "unexpected body structure: %T", structure.Children[1])
	requireTestConditionf(t, uint64(pdf.Size) == uint64(len(attachment)), "pdf %+v", pdf)
	requireTestConditionf(t, pdf.Filename() == "fatura-€.pdf", "pdf %+v", pdf)
	requireTestConditionf(t, strings.EqualFold(pdf.Encoding, "base64"), "pdf %+v", pdf)
	requireTestConditionf(t, pdf.ID == "<document>", "pdf %+v", pdf)
	nested, ok := structure.Children[2].(*imap.BodyStructureSinglePart)
	requireTestConditionf(t, ok, "unexpected embedded structure: %T", structure.Children[2])
	msg := nested.MessageRFC822
	requireTestConditionf(t, msg != nil, "embedded %+v", msg)
	requireTestConditionf(t, msg.Envelope.Subject == "Nested invoice", "embedded %+v", msg)
	requireTestConditionf(t, msg.NumLines == int64(strings.Count(embedded, "\n")), "embedded %+v", msg)
}

type mimeSectionExpectation struct {
	item *imap.FetchItemBodySection
	want string
}

func fetchMIMESections(
	t *testing.T,
	c *imapclient.Client,
	tests []mimeSectionExpectation,
) *imapclient.FetchMessageBuffer {
	t.Helper()
	var items []*imap.FetchItemBodySection
	for _, tc := range tests {
		tc.item.Peek = true
		items = append(items, tc.item)
	}
	data, err := c.Fetch(
		imap.UIDSetNum(1),
		&imap.FetchOptions{
			BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
			BodySection:   items,
			Flags:         true,
		},
	).
		Collect()
	requireTestCondition(t, err == nil, data, err)
	requireTestCondition(t, len(data) == 1, data, err)
	for i, tc := range tests {
		{
			got := string(data[0].FindBodySection(tc.item))
			requireTestConditionf(t, got == tc.want, "section %d: got %q want %q", i, got, tc.want)
		}
	}
	for _, flag := range data[0].Flags {
		requireTestCondition(t, flag != imap.FlagSeen, "PEEK marked seen")
	}
	return data[0]
}
