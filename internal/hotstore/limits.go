//go:build linux

package hotstore

import "fmt"

// EncodedRecordBytes returns the exact framed size for a valid message length.
func EncodedRecordBytes(size int64) (int64, error) {
	if size < 0 || size > maxMessageSize {
		return 0, fmt.Errorf("hotstore: invalid message size")
	}
	return encodedSize(size), nil
}

// ValidateOptions checks the legacy profile or an explicitly capped file.
func ValidateOptions(opts Options) error {
	if opts.MaxMessageBytes <= 0 || opts.MaxMessageBytes > maxMessageSize || opts.MaxBytes < fileHeaderSize ||
		opts.MaxConcurrentAppends <= 0 {
		return fmt.Errorf("hotstore: invalid options")
	}
	if opts.MaxRecords == 0 && opts.MaxFileBytes == 0 {
		return nil
	}
	if opts.MaxRecords <= 0 || opts.MaxFileBytes < fileHeaderSize ||
		encodedSize(opts.MaxMessageBytes) > opts.MaxFileBytes-fileHeaderSize {
		return fmt.Errorf("hotstore: invalid file limits")
	}
	return nil
}
