//go:build linux

package imapserver

import (
	"slices"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
)

func TestIMAPKeywordsAppendSearchCopyAndCaseInsensitiveRemoval(t *testing.T) {
	f := setup(t)
	c := f.client(t)
	command := c.Append(
		"INBOX",
		int64(len(original)),
		&imap.AppendOptions{Flags: []imap.Flag{"Processed", "processed", "$Forwarded"}},
	)
	{
		_, err := command.Write([]byte(original))
		requireTestCondition(t, err == nil, err)
	}
	{
		err := command.Close()
		requireTestCondition(t, err == nil, err)
	}
	{
		_, err := command.Wait()
		requireTestCondition(t, err == nil, err)
	}
	selected, err := c.Select("INBOX", nil).Wait()
	requireTestCondition(
		t,
		(((err == nil && slices.Contains(selected.PermanentFlags, imap.FlagWildcard)) &&
			slices.Contains(selected.Flags, "processed")) &&
			slices.ContainsFunc(
				selected.Flags,
				func(flag imap.Flag) bool {
					return strings.EqualFold(string(flag), "$forwarded")
				},
			)),
		selected,
		err,
	)
	result, err := c.UIDSearch(&imap.SearchCriteria{Flag: []imap.Flag{"PROCESSED"}}, nil).Wait()
	requireTestCondition(t, (err == nil && slices.Equal(result.AllUIDs(), []imap.UID{1})), result, err)
	{
		err := c.Create("Archive", nil).Wait()
		requireTestCondition(t, err == nil, err)
	}
	{
		_, err := c.Copy(imap.UIDSetNum(1), "Archive").Wait()
		requireTestCondition(t, err == nil, err)
	}
	{
		_, err := c.Store(
			imap.UIDSetNum(1),
			&imap.StoreFlags{Op: imap.StoreFlagsDel, Flags: []imap.Flag{"Processed"}},
			nil,
		).
			Collect()
		requireTestCondition(t, err == nil, err)
	}
	result, err = c.UIDSearch(&imap.SearchCriteria{Flag: []imap.Flag{"processed"}}, nil).Wait()
	requireTestCondition(t, (err == nil && len(result.AllUIDs()) == 0), result, err)
	other := f.client(t)
	selected, err = other.Select("Archive", nil).Wait()
	requireTestCondition(t, (err == nil && slices.Contains(selected.Flags, "processed")), selected, err)
	data, err := other.Fetch(imap.UIDSetNum(1), &imap.FetchOptions{Flags: true}).Collect()
	requireTestCondition(t, ((err == nil && len(data) == 1) && slices.Contains(data[0].Flags, "processed")), data, err)
}
