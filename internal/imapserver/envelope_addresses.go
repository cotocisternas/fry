//go:build linux

package imapserver

import (
	"fmt"
	"mime"
	"net/mail"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-message/charset"
)

// net/mail parses individual addresses. The bounded lexical state retains
// group markers and shields delimiters inside comments, quotes and literals.
type envelopeAddressParser struct {
	parser                            mail.AddressParser
	result                            []imap.Address
	start, comments, angles, brackets int
	quoted, escaped, group            bool
}

func envelopeAddresses(value string) ([]imap.Address, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	p := envelopeAddressParser{
		parser: mail.AddressParser{WordDecoder: &mime.WordDecoder{CharsetReader: charset.Reader}},
	}
	if err := p.scan(value); err != nil {
		return nil, err
	}
	if !p.complete() {
		return nil, fmt.Errorf("imap: unterminated envelope address")
	}
	if err := p.add(value[p.start:]); err != nil {
		return nil, err
	}
	return p.result, nil
}

func (p *envelopeAddressParser) scan(value string) error {
	for i := 0; i < len(value); i++ {
		protected, err := p.protected(value[i])
		if err != nil {
			return err
		}
		if protected {
			continue
		}
		if end := encodedWordEnd(value, i); end > i {
			i = end
			continue
		}
		inside, err := p.delimiter(value[i])
		if err != nil {
			return err
		}
		if inside {
			continue
		}
		if err = p.separator(value, i); err != nil {
			return err
		}
	}
	return nil
}

func (p *envelopeAddressParser) protected(c byte) (bool, error) {
	if p.escaped {
		p.escaped = false
		return true, nil
	}
	if (p.quoted || p.comments > 0) && c == '\\' {
		p.escaped = true
		return true, nil
	}
	if p.comments > 0 {
		return true, p.comment(c)
	}
	if p.quoted {
		if c == '"' {
			p.quoted = false
		}
		return true, nil
	}
	if c == '"' {
		p.quoted = true
		return true, nil
	}
	if c == '(' {
		p.comments = 1
		return true, nil
	}
	return false, nil
}

func (p *envelopeAddressParser) comment(c byte) error {
	if c == '(' {
		p.comments++
		if p.comments > 16 {
			return fmt.Errorf("imap: address comments too deeply nested")
		}
	}
	if c == ')' {
		p.comments--
	}
	return nil
}

func encodedWordEnd(value string, i int) int {
	if value[i] != '=' || i+1 >= len(value) || value[i+1] != '?' {
		return i
	}
	if end := strings.Index(value[i+2:], "?="); end >= 0 {
		return i + end + 3
	}
	return i
}

func (p *envelopeAddressParser) delimiter(c byte) (bool, error) {
	switch c {
	case '<':
		p.angles++
	case '>':
		p.angles--
	case '[':
		p.brackets++
	case ']':
		p.brackets--
	}
	if !p.validDelimiters() {
		return false, fmt.Errorf("imap: invalid address delimiters")
	}
	return p.angles > 0 || p.brackets > 0, nil
}

func (p *envelopeAddressParser) validDelimiters() bool {
	return p.angles >= 0 && p.angles <= 1 && p.brackets >= 0 && p.brackets <= 1
}

func (p *envelopeAddressParser) complete() bool {
	return !p.quoted && !p.escaped && p.comments == 0 && p.angles == 0 && p.brackets == 0 && !p.group
}

func (p *envelopeAddressParser) add(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parsed, err := p.parser.Parse(raw)
	if err != nil {
		return err
	}
	at := strings.LastIndexByte(parsed.Address, '@')
	if at < 1 || at == len(parsed.Address)-1 {
		return fmt.Errorf("imap: invalid envelope address")
	}
	p.result = append(
		p.result,
		imap.Address{Name: parsed.Name, Mailbox: parsed.Address[:at], Host: parsed.Address[at+1:]},
	)
	return nil
}

func (p *envelopeAddressParser) separator(value string, i int) error {
	switch value[i] {
	case ':':
		if err := p.startGroup(value[p.start:i]); err != nil {
			return err
		}
		p.start = i + 1
	case ',':
		if err := p.add(value[p.start:i]); err != nil {
			return err
		}
		p.start = i + 1
	case ';':
		if err := p.endGroup(value[p.start:i]); err != nil {
			return err
		}
		p.start = i + 1
	}
	if len(p.result) > 1024 {
		return fmt.Errorf("imap: too many envelope addresses")
	}
	return nil
}

func (p *envelopeAddressParser) startGroup(phrase string) error {
	if p.group {
		return fmt.Errorf("imap: nested address group")
	}
	phrase = strings.TrimSpace(phrase)
	if phrase == "" {
		return fmt.Errorf("imap: empty group name")
	}
	name, err := p.parser.Parse(phrase + " <group@fry.invalid>")
	if err != nil || name.Name == "" {
		return fmt.Errorf("imap: invalid group name")
	}
	p.result = append(p.result, imap.Address{Mailbox: name.Name})
	p.group = true
	return nil
}

func (p *envelopeAddressParser) endGroup(raw string) error {
	if !p.group {
		return fmt.Errorf("imap: unmatched group terminator")
	}
	if err := p.add(raw); err != nil {
		return err
	}
	p.result = append(p.result, imap.Address{})
	p.group = false
	return nil
}
