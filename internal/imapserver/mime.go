//go:build linux

package imapserver

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"mime"
	"net/mail"
	"strings"

	"github.com/cotocisternas/fry/internal/messageindex"

	"github.com/emersion/go-imap/v2"
)

// mimeStructure operates on a completely validated, bounded index before any
// FETCH output is started. Encoded sizes and section bytes share the same spans.
func mimeStructure(p *messageindex.Part) (imap.BodyStructure, error) {
	m, err := mail.ReadMessage(bufio.NewReader(bytes.NewReader(p.Header)))
	if err != nil {
		return nil, err
	}
	var disposition *imap.BodyStructureDisposition
	if value := m.Header.Get("Content-Disposition"); value != "" {
		disposition, err = mimeDisposition(value)
		if err != nil {
			return nil, err
		}
	}
	languages, err := mimeLanguages(m.Header)
	if err != nil {
		return nil, err
	}
	return mimePartStructure(p, m.Header, disposition, languages)
}

func mimePartStructure(
	p *messageindex.Part,
	header mail.Header,
	disposition *imap.BodyStructureDisposition,
	languages []string,
) (imap.BodyStructure, error) {
	typeName, subtype, _ := strings.Cut(p.MediaType, "/")
	if typeName == "multipart" {
		result := &imap.BodyStructureMultiPart{
			Subtype: subtype,
			Extended: &imap.BodyStructureMultiPartExt{
				Params:      p.Params,
				Disposition: disposition,
				Language:    languages,
				Location:    header.Get("Content-Location"),
			},
		}
		for _, child := range p.Children {
			structure, err := mimeStructure(child)
			if err != nil {
				return nil, err
			}
			result.Children = append(result.Children, structure)
		}
		return result, nil
	}
	size := p.End - p.BodyStart
	if size < 0 || size > 4294967295 {
		return nil, fmt.Errorf("imap: MIME part size exceeds wire range")
	}
	result := &imap.BodyStructureSinglePart{
		Type:        typeName,
		Subtype:     subtype,
		Params:      p.Params,
		ID:          header.Get("Content-ID"),
		Description: header.Get("Content-Description"),
		Encoding:    p.Encoding,
		Size:        uint32(size),
		Extended: &imap.BodyStructureSinglePartExt{
			Disposition: disposition,
			Language:    languages,
			Location:    header.Get("Content-Location"),
		},
	}
	if typeName == "text" {
		result.Text = &imap.BodyStructureText{NumLines: p.BodyLines}
	}
	if p.Message != nil {
		embedded, err := embeddedStructure(p)
		if err != nil {
			return nil, err
		}
		result.MessageRFC822 = embedded

	}
	return result, nil
}

var errPartAbsent = errors.New("imap: absent part")

func mimePart(root *messageindex.Part, path []int) (*messageindex.Part, error) {
	p := root
	for i, number := range path {
		if number < 1 {
			return nil, fmt.Errorf("imap: invalid part number")
		}
		messageRoot := i == 0
		if i > 0 && p.Message != nil {
			p = p.Message
			messageRoot = true
		}
		if len(p.Children) > 0 {
			if number > len(p.Children) {
				return nil, errPartAbsent
			}
			p = p.Children[number-1]
		} else if !messageRoot || number != 1 {
			return nil, errPartAbsent
		}
	}
	return p, nil
}

type bodySpan struct {
	offset, length int64
	filtered       []byte
	absent         bool
}

// bodySection resolves a MIME section to original bytes or a bounded filtered
// header. Absent sections remain distinct from existing empty sections.
func bodySection(root *messageindex.Part, item *imap.FetchItemBodySection) (bodySpan, error) {
	p, err := mimePart(root, item.Part)
	if errors.Is(err, errPartAbsent) {
		return bodySpan{absent: true}, nil
	}
	if err != nil {
		return bodySpan{}, err
	}
	if len(item.Part) > 0 && (item.Specifier == imap.PartSpecifierHeader || item.Specifier == imap.PartSpecifierText) {
		if p.Message == nil {
			return bodySpan{absent: true}, nil
		}
		p = p.Message
	}
	return sectionSpan(p, item)
}

func sectionSpan(p *messageindex.Part, item *imap.FetchItemBodySection) (bodySpan, error) {
	span := bodySpan{offset: p.BodyStart, length: p.End - p.BodyStart}
	switch item.Specifier {
	case imap.PartSpecifierNone:
		if len(item.Part) == 0 {
			span.offset = p.HeaderStart
			span.length = p.End - p.HeaderStart
		}
	case imap.PartSpecifierMIME, imap.PartSpecifierHeader:
		span.offset, span.length = p.HeaderStart, int64(len(p.Header))
		if len(item.HeaderFields) > 0 || len(item.HeaderFieldsNot) > 0 {
			span.filtered = filterHeader(p.Header, item.HeaderFields, item.HeaderFieldsNot)
			span.offset, span.length = 0, int64(len(span.filtered))
		}
	case imap.PartSpecifierText:
	default:
		return bodySpan{}, fmt.Errorf("imap: unknown section")
	}
	if partial := item.Partial; partial != nil {
		skip := min(partial.Offset, span.length)
		span.offset += skip
		span.length = min(partial.Size, span.length-skip)
	}
	return span, nil
}

func mimeDisposition(value string) (*imap.BodyStructureDisposition, error) {
	kind, params, err := mime.ParseMediaType(value)
	if err != nil || len(params) > 128 {
		return nil, fmt.Errorf("imap: invalid or excessive disposition parameters")
	}
	return &imap.BodyStructureDisposition{Value: kind, Params: params}, nil
}

func mimeLanguages(header mail.Header) ([]string, error) {
	var languages []string
	if value := header.Get("Content-Language"); value != "" {
		if strings.Count(value, ",") >= 128 {
			return nil, fmt.Errorf("imap: excessive content languages")
		}
		for language := range strings.SplitSeq(value, ",") {
			languages = append(languages, strings.TrimSpace(language))
		}
	}
	return languages, nil
}

func embeddedStructure(p *messageindex.Part) (*imap.BodyStructureMessageRFC822, error) {
	// The pinned writer supports embedded RFC822 envelopes; message/global
	// remains unsupported until the client's extension grammar is covered.
	if p.MediaType != "message/rfc822" {
		return nil, fmt.Errorf("imap: message/global BODYSTRUCTURE unsupported")
	}
	e, err := envelope(p.Message.Header)
	if err != nil {
		return nil, err
	}
	structure, err := mimeStructure(p.Message)
	if err != nil {
		return nil, err
	}
	return &imap.BodyStructureMessageRFC822{Envelope: e, BodyStructure: structure, NumLines: p.BodyLines}, nil
}
