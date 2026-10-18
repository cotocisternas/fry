//go:build linux

package imapserver

import (
	"sort"
	"strings"

	"github.com/cotocisternas/fry/internal/receiptstore"

	"github.com/emersion/go-imap/v2"
	wire "github.com/emersion/go-imap/v2/imapserver"
)

func listSupported(o *imap.ListOptions) bool {
	return !o.SelectRemote && !o.SelectRecursiveMatch && !o.SelectSpecialUse && !o.ReturnChildren &&
		o.ReturnStatus == nil &&
		!o.ReturnSpecialUse
}

func listEntries(
	folders []receiptstore.ListedFolder,
	ref string,
	patterns []string,
	lsub bool,
) map[string]receiptstore.ListedFolder {
	entries := map[string]receiptstore.ListedFolder{}
	for _, folder := range folders {
		entries[folder.Name] = folder
	}
	if lsub {
		addSubscribedParents(entries, folders, ref, patterns)
	}
	return entries
}

func addSubscribedParents(
	entries map[string]receiptstore.ListedFolder,
	folders []receiptstore.ListedFolder,
	ref string,
	patterns []string,
) {
	// Legacy LSUB '%' includes unsubscribed parents as Noselect. Ordinary
	// LIST (SUBSCRIBED) deliberately keeps its distinct selection semantics.
	for _, folder := range folders {
		for name := folder.Name; strings.Contains(name, "/"); {
			name = name[:strings.LastIndexByte(name, '/')]
			if _, exists := entries[name]; !exists && matchesSubscribedParent(name, ref, patterns) {
				entries[name] = receiptstore.ListedFolder{Name: name}
			}
		}
	}
}

func matchesSubscribedParent(name, ref string, patterns []string) bool {
	for _, pattern := range patterns {
		if strings.Contains(pattern, "%") && wire.MatchList(name, '/', ref, pattern) {
			return true
		}
	}
	return false
}

func matchesList(name, ref string, patterns []string) bool {
	for _, pattern := range patterns {
		if wire.MatchList(name, '/', ref, pattern) {
			return true
		}
	}
	return false
}

func writeListEntries(
	w *wire.ListWriter,
	ref string,
	patterns []string,
	o *imap.ListOptions,
	entries map[string]receiptstore.ListedFolder,
) error {
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !matchesList(name, ref, patterns) {
			continue
		}
		folder := entries[name]
		data := &imap.ListData{Mailbox: name, Delim: '/'}
		if !folder.Selectable {
			data.Attrs = append(data.Attrs, imap.MailboxAttrNoSelect)
		}
		if o.ReturnSubscribed && folder.Subscribed {
			data.Attrs = append(data.Attrs, imap.MailboxAttrSubscribed)
		}
		if err := w.WriteList(data); err != nil {
			return err
		}
	}
	return nil
}
