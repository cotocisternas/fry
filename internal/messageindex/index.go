// Package messageindex indexes MIME structure as spans of the original encoded
// message. It never decodes or buffers attachment bodies. Parsing errors affect
// metadata access only; callers retain the original message independently.
package messageindex

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"net/mail"
	"slices"
	"strings"
)

const (
	MaxHeaderBytes       = 1 << 20
	MaxTotalHeaderBytes  = 8 << 20
	MaxParts             = 1024
	MaxDepth             = 32
	MaxHeaderFields      = 1024
	MaxTotalHeaderFields = 8192
)

// Part spans [HeaderStart, End); BodyStart excludes the terminating blank header
// line. End excludes the CRLF owned by an enclosing multipart delimiter. Header
// is bounded original data; BodyLines counts LF bytes in the encoded body.
type Part struct {
	HeaderStart, BodyStart, End, BodyLines int64
	Header                                 []byte
	MediaType                              string
	Params                                 map[string]string
	Encoding                               string
	Children                               []*Part
	Message                                *Part
}

// Parse consumes exactly size bytes, using bounded metadata and a fixed body
// buffer. The caller owns r, must close it, and must make blocked reads obey ctx.
// No index is returned for truncated, ambiguous, or over-budget structure.
func Parse(ctx context.Context, r io.Reader, size int64) (*Part, error) {
	if size < 0 || size > 1<<32-1 {
		return nil, fmt.Errorf("messageindex: invalid size")
	}
	s := &scanner{ctx: ctx, reader: bufio.NewReaderSize(io.LimitReader(r, size+1), 32<<10)}
	p, err := s.entity(nil, 0, "text/plain")
	if err != nil {
		return nil, err
	}
	if s.offset != size {
		return nil, fmt.Errorf("messageindex: size mismatch")
	}
	return p, nil
}

type line struct {
	start, end, beforeLines, previousEOL int64
	data                                 []byte
	padding                              bool
	eol                                  int64
}
type scanner struct {
	ctx                        context.Context
	reader                     *bufio.Reader
	offset, lines, eol         int64
	headerBytes, parts, fields int
	pending                    *line
}

// Body lines retain only a prefix long enough for the maximum MIME delimiter.
// A long transport-padding suffix is validated without retaining it.
func (s *scanner) next(header bool) (*line, error) {
	if s.pending != nil {
		l := s.pending
		s.pending = nil
		return l, nil
	}
	l := &line{start: s.offset, beforeLines: s.lines, previousEOL: s.eol, padding: true}
	limit := 256
	if header {
		limit = MaxHeaderBytes
	}
	state := lineScan{limit: limit, header: header}
	if err := s.readLine(l, &state); err != nil {
		return nil, err
	}
	if state.tailCR {
		l.padding = false
	}
	s.eol = l.eol
	return l, nil
}

func (s *scanner) readLine(l *line, state *lineScan) error {
	for {
		if err := s.ctx.Err(); err != nil {
			return err
		}
		b, err := s.reader.ReadSlice('\n')
		if appendErr := state.append(l, b); appendErr != nil {
			return appendErr
		}
		s.lines += int64(bytes.Count(b, []byte{'\n'}))
		s.offset += int64(len(b))
		l.end = s.offset
		// Only bufio's exact continuation sentinel permits another fragment; wrapped source errors must propagate.
		if err == bufio.ErrBufferFull { //nolint:errorlint // Preserve exact sentinel identity.
			continue
		}
		if err != nil && err != io.EOF {
			return err
		}
		if err == io.EOF && l.end == l.start {
			return io.EOF
		}
		return nil
	}
}

// lineScan retains a bounded prefix while tracking delimiter padding and EOLs
// across bufio fragments, including a CR split from its following LF.
type lineScan struct {
	limit          int
	header, tailCR bool
	previous       byte
}

func (state *lineScan) append(l *line, fragment []byte) error {
	for _, c := range fragment {
		if err := state.appendByte(l, c); err != nil {
			return err
		}
		if c == '\n' {
			l.eol = 1
			if state.previous == '\r' {
				l.eol = 2
			}
		}
		state.previous = c
	}
	return nil
}

func (state *lineScan) appendByte(l *line, c byte) error {
	if len(l.data) < state.limit {
		l.data = append(l.data, c)
		return nil
	}
	if state.header {
		return fmt.Errorf("messageindex: header line limit")
	}
	if state.tailCR && c != '\n' {
		l.padding = false
	}
	if c != ' ' && c != '\t' && c != '\r' && c != '\n' {
		l.padding = false
	}
	state.tailCR = c == '\r'
	return nil
}

func delimiter(l *line, boundaries []string) (int, bool) {
	if !l.padding {
		return -1, false
	}
	b := l.data
	if int64(len(b)) == l.end-l.start {
		b = bytes.TrimSuffix(b, []byte("\n"))
		if l.eol == 2 {
			b = bytes.TrimSuffix(b, []byte("\r"))
		}
	} else if l.eol == 2 && l.end-l.start == int64(len(b))+1 {
		// The retained prefix ends exactly between the final CR and LF.
		b = bytes.TrimSuffix(b, []byte("\r"))
	}
	b = bytes.TrimRight(b, " \t")
	for i := len(boundaries) - 1; i >= 0; i-- {
		if string(b) == "--"+boundaries[i] {
			return i, false
		}
		if string(b) == "--"+boundaries[i]+"--" {
			return i, true
		}
	}
	return -1, false
}

func (s *scanner) entity(boundaries []string, depth int, defaultType string) (*Part, error) {
	s.parts++
	if depth > MaxDepth || s.parts > MaxParts {
		return nil, fmt.Errorf("messageindex: structure limit")
	}
	p := &Part{HeaderStart: s.offset}
	if err := s.readHeader(p, boundaries); err != nil {
		return nil, err
	}
	p.BodyStart = s.offset
	bodyLines := s.lines
	if err := p.parseHeader(defaultType); err != nil {
		return nil, err
	}
	isMultipart := strings.HasPrefix(p.MediaType, "multipart/")
	isMessage := slices.Contains([]string{"message/rfc822", "message/global"}, p.MediaType)
	if err := p.validateContainerEncoding(isMultipart || isMessage); err != nil {
		return nil, err
	}
	if isMessage {
		var err error
		p.Message, err = s.entity(boundaries, depth+1, "text/plain")
		if err != nil {
			return nil, err
		}
	}
	active, err := activeBoundaries(p, boundaries)
	if err != nil {
		return nil, err
	}
	return s.readBody(p, boundaries, active, depth, bodyLines, !isMultipart)
}

func (p *Part) validateContainerEncoding(container bool) error {
	if container && p.Encoding != "7bit" && p.Encoding != "8bit" && p.Encoding != "binary" {
		return fmt.Errorf("messageindex: encoded container unsupported")
	}
	return nil
}

// readBody keeps delimiter ownership and multipart close state in one loop.
//
// The bounded delimiter state machine keeps pending-line ownership visible; header, child, and completion parsing
// are separate.
//
//nolint:gocognit // See the rationale above.
func (s *scanner) readBody(
	p *Part,
	boundaries, active []string,
	depth int,
	bodyLines int64,
	closed bool,
) (*Part, error) {
	for {
		l, err := s.next(false)
		// Only exact EOF completes a MIME body; a wrapped source failure must remain an error.
		if err == io.EOF { //nolint:errorlint // Preserve exact sentinel identity.
			return s.finishBody(p, nil, bodyLines, closed)
		}
		if err != nil {
			return nil, err
		}
		i, closing := delimiter(l, active)
		if i < 0 {
			continue
		}
		if i < len(boundaries) {
			return s.finishBody(p, l, bodyLines, closed)
		}
		if closing {
			if len(p.Children) == 0 {
				return nil, fmt.Errorf("messageindex: empty multipart")
			}
			closed = true
			active = boundaries // Epilogue is opaque, including boundary-like lines.
			continue
		}
		if err := s.appendChild(p, active, depth); err != nil {
			return nil, err
		}
	}
}

func (s *scanner) appendChild(p *Part, active []string, depth int) error {
	childDefault := "text/plain"
	if p.MediaType == "multipart/digest" {
		childDefault = "message/rfc822"
	}
	child, err := s.entity(active, depth+1, childDefault)
	if err != nil {
		return err
	}
	p.Children = append(p.Children, child)
	return nil
}

func (s *scanner) finishBody(p *Part, boundary *line, bodyLines int64, closed bool) (*Part, error) {
	if !closed {
		return nil, fmt.Errorf("messageindex: missing closing delimiter")
	}
	if boundary == nil {
		p.End, p.BodyLines = s.offset, s.lines-bodyLines
		return p, nil
	}
	s.pending = boundary
	p.End = max(p.BodyStart, boundary.start-boundary.previousEOL)
	p.BodyLines = boundary.beforeLines - bodyLines
	if boundary.previousEOL > 0 && boundary.start > p.BodyStart {
		p.BodyLines--
	}
	return p, nil
}

func (s *scanner) readHeader(p *Part, boundaries []string) error {
	fields := 0
	for {
		l, err := s.next(true)
		if err != nil {
			return fmt.Errorf("messageindex: incomplete header: %w", err)
		}
		if i, _ := delimiter(l, boundaries); i >= 0 {
			return fmt.Errorf("messageindex: delimiter inside header")
		}
		if err := s.appendHeaderLine(p, l); err != nil {
			return err
		}
		if bytes.Equal(l.data, []byte("\r\n")) || bytes.Equal(l.data, []byte("\n")) {
			break
		}
		if err := s.countHeaderField(l, &fields); err != nil {
			return err
		}
	}
	return nil
}

func (s *scanner) countHeaderField(l *line, fields *int) error {
	if l.data[0] == ' ' || l.data[0] == '\t' {
		return nil
	}
	*fields++
	s.fields++
	if *fields > MaxHeaderFields || s.fields > MaxTotalHeaderFields {
		return fmt.Errorf("messageindex: header field limit")
	}
	return nil
}

func (s *scanner) appendHeaderLine(p *Part, l *line) error {
	if len(p.Header)+len(l.data) > MaxHeaderBytes || s.headerBytes+len(l.data) > MaxTotalHeaderBytes {
		return fmt.Errorf("messageindex: header budget")
	}
	p.Header = append(p.Header, l.data...)
	s.headerBytes += len(l.data)
	return nil
}

func (p *Part) parseHeader(defaultType string) error {
	m, err := mail.ReadMessage(bufio.NewReader(bytes.NewReader(p.Header)))
	if err != nil {
		return err
	}
	for _, key := range []string{"Content-Type", "Content-Transfer-Encoding"} {
		if len(m.Header[key]) > 1 {
			return fmt.Errorf("messageindex: ambiguous %s", key)
		}
	}
	if err := p.parseContentType(m.Header, defaultType); err != nil {
		return err
	}
	p.Encoding = strings.ToLower(strings.TrimSpace(m.Header.Get("Content-Transfer-Encoding")))
	if p.Encoding == "" {
		p.Encoding = "7bit"
	}
	return nil
}

func (p *Part) parseContentType(header mail.Header, defaultType string) error {
	value := header.Get("Content-Type")
	if value == "" {
		value = defaultType
	}
	var err error
	p.MediaType, p.Params, err = mime.ParseMediaType(value)
	if err != nil || !strings.Contains(p.MediaType, "/") || len(p.Params) > 128 {
		return fmt.Errorf("messageindex: invalid content type")
	}
	if p.MediaType == "text/plain" && p.Params["charset"] == "" {
		p.Params["charset"] = "us-ascii"
	}
	return nil
}

func activeBoundaries(p *Part, boundaries []string) ([]string, error) {
	if !strings.HasPrefix(p.MediaType, "multipart/") {
		return boundaries, nil
	}
	boundary := p.Params["boundary"]
	if !validBoundary(boundary) {
		return nil, fmt.Errorf("messageindex: invalid boundary")
	}
	if slices.Contains(boundaries, boundary) {
		return nil, fmt.Errorf("messageindex: repeated enclosing boundary")
	}
	return append(append([]string(nil), boundaries...), boundary), nil
}

func validBoundary(b string) bool {
	if len(b) == 0 || len(b) > 70 || b[len(b)-1] == ' ' {
		return false
	}
	for _, c := range b {
		if boundaryCharacter(c) {
			continue
		}
		return false
	}
	return true
}

func boundaryCharacter(c rune) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		strings.ContainsRune("'()+_,-./:=? ", c)
}
