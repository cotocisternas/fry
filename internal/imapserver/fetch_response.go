//go:build linux

package imapserver

import (
	"bytes"
	"context"
	"io"

	"github.com/cotocisternas/fry/internal/messageindex"
	"github.com/cotocisternas/fry/internal/receiptstore"

	"github.com/emersion/go-imap/v2"
	wire "github.com/emersion/go-imap/v2/imapserver"
)

type fetchPlan struct{ markSeen, needsHeader, needsIndex bool }

func (s *session) planFetch(o *imap.FetchOptions) (fetchPlan, error) {
	plan := fetchPlan{needsHeader: o.Envelope, needsIndex: o.BodyStructure != nil}
	if len(o.BinarySection) > 0 || len(o.BinarySectionSize) > 0 || o.ModSeq || o.ChangedSince != 0 {
		return plan, unsupported()
	}
	for _, section := range o.BodySection {
		if err := plan.includeSection(section, s.readOnly); err != nil {
			return plan, err
		}
	}

	return plan, nil
}

func validateFetchSection(section *imap.FetchItemBodySection) error {
	if !validHeaderSelection(section) {
		return unsupported()
	}
	switch section.Specifier {
	case imap.PartSpecifierNone, imap.PartSpecifierHeader, imap.PartSpecifierText:
	case imap.PartSpecifierMIME:
		if len(section.Part) == 0 {
			return unsupported()
		}
	default:
		return unsupported()
	}
	if section.Partial != nil && (section.Partial.Offset < 0 || section.Partial.Size < 0) {
		return unsupported()
	}
	return nil
}

func validHeaderSelection(section *imap.FetchItemBodySection) bool {
	include, exclude := len(section.HeaderFields) > 0, len(section.HeaderFieldsNot) > 0
	return (!include || !exclude) && ((!include && !exclude) || section.Specifier == imap.PartSpecifierHeader)
}

type fetchMetadata struct {
	spans     []bodySpan
	structure imap.BodyStructure
	envelope  *imap.Envelope
}

func (s *session) fetchIndex(ctx context.Context, m receiptstore.Mail, plan fetchPlan) (*messageindex.Part, error) {
	var header []byte
	if plan.needsIndex {
		reader, err := s.server.store.Open(ctx, s.account.org, m.Receipt, 0, m.Size)
		if err != nil {
			return nil, safe(err)
		}
		index, parseErr := messageindex.Parse(ctx, reader, m.Size)
		closeErr := reader.Close()
		if parseErr != nil || closeErr != nil {
			return nil, no("MIME metadata unavailable within prototype parsing limits")
		}
		return index, nil
	}
	if plan.needsHeader {
		var err error
		header, _, err = s.headers(ctx, m)
		if err != nil {
			return nil, headerFailure(ctx, err)
		}
	}
	return &messageindex.Part{Header: header, BodyStart: int64(len(header)), End: m.Size}, nil
}

func (s *session) prepareFetch(
	ctx context.Context,
	m receiptstore.Mail,
	o *imap.FetchOptions,
	plan fetchPlan,
) (fetchMetadata, error) {
	var data fetchMetadata
	index, err := s.fetchIndex(ctx, m, plan)
	if err != nil {
		return data, err
	}
	// Resolve every requested section before emitting any message response.
	data.spans = make([]bodySpan, len(o.BodySection))
	for j, section := range o.BodySection {
		data.spans[j], err = bodySection(index, section)
		if err != nil {
			return data, no("Requested MIME section unavailable")
		}
	}
	if o.BodyStructure != nil {
		data.structure, err = mimeStructure(index)
		if err != nil {
			return data, no("Body structure unavailable within prototype parsing limits")
		}
	}
	if o.Envelope {
		data.envelope, err = envelope(index.Header)
		if err != nil {
			return data, no("Envelope unavailable within prototype parsing limits")
		}
	}
	return data, nil
}

func (s *session) writeFetch(
	ctx context.Context,
	w *wire.FetchWriter,
	seq uint32,
	m receiptstore.Mail,
	o *imap.FetchOptions,
	plan fetchPlan,
	data fetchMetadata,
) error {
	response := w.CreateMessage(seq)
	if o.BodyStructure != nil {
		response.WriteBodyStructure(data.structure)
	}
	if o.Envelope {
		response.WriteEnvelope(data.envelope)
	}
	if o.UID {
		response.WriteUID(imap.UID(m.UID))
	}
	if o.Flags || plan.markSeen {
		response.WriteFlags(flags(m))
		s.view[seq-1] = m
	}
	if o.InternalDate {
		response.WriteInternalDate(m.Date)
	}
	if o.RFC822Size {
		response.WriteRFC822Size(m.Size)
	}
	for j, section := range o.BodySection {
		if err := s.writeFetchSection(ctx, response, m, section, data.spans[j]); err != nil {
			return err
		}
	}
	return response.Close()
}

func (s *session) writeFetchSection(
	ctx context.Context,
	response *wire.FetchResponseWriter,
	m receiptstore.Mail,
	section *imap.FetchItemBodySection,
	span bodySpan,
) error {
	if span.absent {
		response.WriteBodySectionNil(section)
		return nil
	}
	reader, err := s.openFetchSpan(ctx, m, span)
	if err != nil {
		closeAfterFailure(response)
		return safe(err)
	}
	literal := response.WriteBodySection(section, span.length)
	written, copyErr := io.Copy(literal, reader)
	closeErr := reader.Close()
	if copyErr != nil || closeErr != nil || written != span.length {
		closeAfterFailure(s.conn.NetConn())
		closeAfterFailure(literal)
		closeAfterFailure(response)
		return no("Message transfer interrupted")
	}
	if err = literal.Close(); err != nil {
		closeAfterFailure(s.conn.NetConn())
		closeAfterFailure(response)
		return err
	}
	return nil
}

func (s *session) openFetchSpan(ctx context.Context, m receiptstore.Mail, span bodySpan) (io.ReadCloser, error) {
	if span.filtered != nil {
		return io.NopCloser(bytes.NewReader(span.filtered[span.offset : span.offset+span.length])), nil
	}
	return s.server.store.Open(ctx, s.account.org, m.Receipt, span.offset, span.length)
}

func (p *fetchPlan) includeSection(section *imap.FetchItemBodySection, readOnly bool) error {
	if err := validateFetchSection(section); err != nil {
		return err
	}
	if len(section.Part) > 0 {
		p.needsIndex = true
	}
	if section.Specifier == imap.PartSpecifierHeader || section.Specifier == imap.PartSpecifierText {
		p.needsHeader = true
	}
	if !section.Peek && !readOnly {
		p.markSeen = true
	}
	return nil
}
