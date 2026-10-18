package imapserver

import (
	"fmt"
	"io"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

// defaultAppendLimit is the default maximum size of an APPEND payload.
const defaultAppendLimit = 100 * 1024 * 1024 // 100MiB

func (c *Conn) handleAppend(tag string, dec *imapwire.Decoder) error {
	var (
		mailbox string
		options imap.AppendOptions
	)
	if !dec.ExpectSP() || !dec.ExpectMailbox(&mailbox) || !dec.ExpectSP() {
		return dec.Err()
	}

	hasFlagList, err := dec.List(func() error {
		flag, err := internal.ExpectFlag(dec)
		if err != nil {
			return err
		}
		options.Flags = append(options.Flags, flag)
		return nil
	})
	if err != nil {
		return err
	}
	if hasFlagList && !dec.ExpectSP() {
		return dec.Err()
	}

	t, err := internal.DecodeDateTime(dec)
	if err != nil {
		return err
	}
	if !t.IsZero() && !dec.ExpectSP() {
		return dec.Err()
	}
	options.Time = t

	var dataExt string
	if !dec.Special('~') && dec.Atom(&dataExt) { // ignore literal8 prefix if any for BINARY
		switch strings.ToUpper(dataExt) {
		case "UTF8":
			// '~' is the literal8 prefix
			if !dec.ExpectSP() || !dec.ExpectSpecial('(') || !dec.ExpectSpecial('~') {
				return dec.Err()
			}
		default:
			return newClientBugError("Unknown APPEND data extension")
		}
	}

	lit, nonSync, err := dec.ExpectLiteralReader()
	if err != nil {
		return err
	}

	appendLimit := int64(defaultAppendLimit)
	if appendLimitSession, ok := c.session.(SessionAppendLimit); ok {
		appendLimit = int64(appendLimitSession.AppendLimit())
	}

	if lit.Size() > appendLimit {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeTooBig,
			Text: fmt.Sprintf("Literals are limited to %v bytes for this command", appendLimit),
		}
	}
	if err := c.acceptLiteral(lit.Size(), nonSync); err != nil {
		return err
	}

	c.setReadTimeout(literalReadTimeout)
	defer c.setReadTimeout(cmdReadTimeout)

	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		io.Copy(io.Discard, lit)
		dec.CRLF()
		return err
	}

	// A backend must not observe the complete literal before its command framing
	// is valid. In particular, CopyN can discard an error on its final full read.
	framed := &appendLiteral{literal: lit, decoder: dec, remaining: lit.Size(), extension: dataExt}
	if framed.remaining == 0 {
		if err := framed.finish(); err != nil {
			return err
		}
	}
	data, appendErr := c.session.Append(mailbox, framed, &options)
	if _, err := io.Copy(io.Discard, framed); err != nil {
		return err
	}

	if appendErr != nil {
		return appendErr
	}
	if err := c.poll("APPEND"); err != nil {
		return err
	}
	return c.writeAppendOK(tag, data)
}

func (c *Conn) writeAppendOK(tag string, data *imap.AppendData) error {
	enc := newResponseEncoder(c)
	defer enc.end()

	enc.Atom(tag).SP().Atom("OK").SP()
	if data != nil {
		enc.Special('[')
		enc.Atom("APPENDUID").SP().Number(data.UIDValidity).SP().UID(data.UID)
		enc.Special(']').SP()
	}
	enc.Text("APPEND completed")
	return enc.CRLF()
}

// appendLiteral withholds the final payload chunk until the literal is complete
// and the command trailer has been decoded. It does not buffer the whole message.
// Only a fully framed command exposes Size() bytes or a successful EOF.
type appendLiteral struct {
	literal   *imapwire.LiteralReader
	decoder   *imapwire.Decoder
	remaining int64
	extension string
	finished  bool
	failure   error
}

func (r *appendLiteral) Size() int64 { return r.literal.Size() }
func (r *appendLiteral) finish() error {
	if r.failure != nil {
		return r.failure
	}
	if r.finished {
		return nil
	}
	// Force the decoder's literal reader to observe EOF and release literal mode.
	var probe [1]byte
	n, err := r.literal.Read(probe[:])
	if n != 0 || err != io.EOF {
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		r.failure = err
		return err
	}
	if r.extension != "" && !r.decoder.ExpectSpecial(')') {
		r.failure = r.decoder.Err()
	}
	if r.failure == nil && !r.decoder.ExpectCRLF() {
		r.failure = r.decoder.Err()
	}
	if r.failure != nil {
		return r.failure
	}
	r.finished = true
	return nil
}
func (r *appendLiteral) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.failure != nil {
		return 0, r.failure
	}
	if r.finished {
		return 0, io.EOF
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.literal.Read(p)
	r.remaining -= int64(n)
	if err != nil && (err != io.EOF || r.remaining > 0) {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		r.failure = err
		if r.remaining == 0 {
			return 0, err
		}
		return n, err
	}
	if r.remaining == 0 {
		if err = r.finish(); err != nil {
			return 0, err
		}
	}
	return n, nil
}
