//go:build linux

package imapserver

import (
	"testing"

	"github.com/emersion/go-imap/v2"
)

func TestIMAPSearchDecodedHeadersAndUnknownCharset(t *testing.T) {
	f := setup(t)
	f.add(
		t,
		"From: =?iso-8859-1?Q?Jos=E9?= <sender@example.test>\r\n"+
			"Subject: =?utf-8?Q?Fatura_n=C3=BAmero_42?=\r\n"+
			"X-Unknown: =?x-fry-unknown?Q?value?=\r\n\r\nbody",
	)
	c := f.client(t)
	{
		_, err := c.Select("INBOX", nil).Wait()
		requireTestCondition(t, err == nil, err)
	}
	for _, query := range []*imap.SearchCriteria{
		{Header: []imap.SearchCriteriaHeaderField{{Key: "From", Value: "JOSÉ"}}},
		{Header: []imap.SearchCriteriaHeaderField{{Key: "Subject", Value: "NÚMERO"}}},
		{Or: [][2]imap.SearchCriteria{{
			{Header: []imap.SearchCriteriaHeaderField{{Key: "Subject", Value: "absent"}}},
			{Header: []imap.SearchCriteriaHeaderField{{Key: "From", Value: "José"}}},
		}}},
		{Not: []imap.SearchCriteria{{Header: []imap.SearchCriteriaHeaderField{{Key: "Subject", Value: "absent"}}}}},
		{Header: []imap.SearchCriteriaHeaderField{{Key: "X-Unknown", Value: ""}}},
	} {
		data, err := c.UIDSearch(query, nil).Wait()
		requireTestCondition(t, ((err == nil && len(data.AllUIDs()) == 1) && data.AllUIDs()[0] == 1), data, err)
	}
	{
		_, err := c.UIDSearch(
			&imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "X-Unknown", Value: "value"}}},
			nil,
		).
			Wait()
		requireTestCondition(t, err != nil, "unknown charset silently searched as raw text")
	}
}
