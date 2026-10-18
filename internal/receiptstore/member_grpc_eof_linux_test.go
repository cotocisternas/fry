//go:build linux

package receiptstore

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"testing"

	memberv1 "github.com/cotocisternas/fry/gen/member/go/fry/member/v1"

	"connectrpc.com/connect/v2"
)

func memberFailureEOFs() map[string]error {
	failure := errors.New("stream failed")
	return map[string]error{
		"wrapped":        fmt.Errorf("wrapped: %w", io.EOF),
		"joined":         errors.Join(io.EOF, failure),
		"wrapped joined": fmt.Errorf("wrapped: %w", errors.Join(io.EOF, failure)),
		"connect joined": connect.NewError(connect.CodeUnknown, "failed").
			WithCause(errors.Join(io.EOF, failure)),
		"wrapped connect": fmt.Errorf(
			"wrapped: %w",
			connect.NewError(connect.CodeUnknown, "EOF").WithCause(io.EOF),
		),
		"connect failure message": connect.NewError(connect.CodeUnknown, "failed").WithCause(io.EOF),
		"remote EOF":              connect.NewError(connect.CodeUnknown, "EOF").WithCause(io.EOF).WithRemote(),
		"remote status":           connect.NewError(connect.CodeInternal, "failed").WithCause(io.EOF),
	}
}

func TestMemberChunkReaderRejectsAdornedTerminalEOF(t *testing.T) {
	for name, terminal := range memberFailureEOFs() {
		t.Run(name, func(t *testing.T) {
			digest := sha256.Sum256([]byte("last"))
			frames := 0
			receive := func() ([]byte, *memberv1.CompressedChunk, *memberv1.Completion, error) {
				frames++
				switch frames {
				case 1:
					return []byte("last"), nil, nil, nil
				case 2:
					return nil, nil, &memberv1.Completion{Size: 4, Sha256: digest[:]}, nil
				default:
					return nil, nil, nil, terminal
				}
			}
			var dst bytes.Buffer
			_, err := io.CopyN(&dst, newMemberChunkReader(receive, 4, nil), 4)
			requireReceiptCondition(
				t,
				errors.Is(err, ErrConflict) && dst.Len() == 0,
				"final bytes exposed after terminal failure",
			)
		})
	}
}

type memberTerminalInflater struct {
	terminal error
	read     bool
}

func (r *memberTerminalInflater) Reset(input io.Reader, _ []byte) error {
	r.read = false
	_, err := io.Copy(io.Discard, input)
	return err
}

func (r *memberTerminalInflater) Read(p []byte) (int, error) {
	if r.read {
		return 0, r.terminal
	}
	r.read = true
	p[0] = 'x'
	return 1, nil
}

func (*memberTerminalInflater) Close() error { return nil }

func TestMemberGRPCDecoderRequiresExactEOF(t *testing.T) {
	terminals := memberFailureEOFs()
	terminals["clean"] = io.EOF
	for name, terminal := range terminals {
		t.Run(name, func(t *testing.T) {
			decoder := memberChunkDecoder{reader: &memberTerminalInflater{terminal: terminal}}
			data, err := decoder.decode(&memberv1.CompressedChunk{Size: 1, Data: []byte{0}})
			if name == "clean" {
				requireReceiptNoError(t, err)
				requireReceiptCondition(t, string(data) == "x", "decoded payload differs")
				return
			}
			requireReceiptCondition(
				t,
				errors.Is(err, ErrConflict) && len(data) == 0,
				"decoder accepted terminal failure",
			)
		})
	}
}

func TestMemberGRPCReceiveNormalizesOnlySDKCleanEOF(t *testing.T) {
	clean := connect.NewError(connect.CodeUnknown, "EOF").WithCause(io.EOF)
	requireReceiptCondition(
		t,
		memberReceiveError(clean) == io.EOF, //nolint:errorlint // Verify the exact normalized sentinel.
		"clean SDK EOF was not normalized",
	)
	for name, terminal := range memberFailureEOFs() {
		t.Run(name, func(t *testing.T) {
			requireReceiptCondition(
				t,
				memberReceiveError(terminal) == terminal, //nolint:errorlint // Preserve original failure identity.
				"terminal failure was normalized away",
			)
		})
	}
}
