//go:build linux

package imapserver

import (
	"bufio"
	"bytes"
	"fmt"
	"log/slog"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-message"
	messageMail "github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"
)

// envelope parses only the already bounded header block. Raw message retrieval
// never uses this parsed representation or reserializes it into stored bytes.
func envelope(raw []byte) (*imap.Envelope, error) {
	header, err := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(raw)))
	if err != nil {
		return nil, err
	}
	mh := messageMail.Header{Header: message.Header{Header: header}}
	result := &imap.Envelope{}
	// Missing or invalid dates are represented as NIL.
	result.Date = optionalEnvelopeValue(mh.Date())
	result.Subject, err = mh.Subject()
	if err != nil {
		return nil, err
	}
	result.InReplyTo = optionalEnvelopeValue(mh.MsgIDList("In-Reply-To"))
	result.MessageID = optionalEnvelopeValue(mh.MessageID())
	destinations := []struct {
		name   string
		target *[]imap.Address
	}{
		{"From", &result.From},
		{"Sender", &result.Sender},
		{"Reply-To", &result.ReplyTo},
		{"To", &result.To},
		{"Cc", &result.Cc},
		{"Bcc", &result.Bcc},
	}
	count := 0
	for _, destination := range destinations {
		var values []string
		fields := header.FieldsByKey(destination.name)
		for fields.Next() {
			values = append(values, fields.Value())
		}
		*destination.target, err = envelopeAddresses(strings.Join(values, ", "))
		if err != nil {
			return nil, err
		}
		count += len(*destination.target)
		if count > 1024 {
			return nil, fmt.Errorf("imap: too many envelope addresses")
		}
	}
	return result, nil
}

// Optional envelope fields retain the parser's best effort value, including
// the valid prefix of an In-Reply-To list with a malformed suffix.
func optionalEnvelopeValue[T any](value T, err error) T {
	if err != nil {
		slog.Debug("IMAP optional envelope field parsing incomplete")
	}
	return value
}
