//go:build linux

package receiptstore

// finishPipe delivers completion/cancellation to the other pipe endpoint. The
// concrete io.Pipe close methods always return nil; no durability proof or
// underlying file/socket ownership is transferred by closing a pipe.
func finishPipe(pipe interface{ CloseWithError(error) error }, cause error) {
	// io.PipeReader/PipeWriter.CloseWithError always return nil. Owner: FRY maintainers; expires: 2027-01-18.
	//nolint:errcheck // See the rationale above.
	_ = pipe.CloseWithError(cause)
}

// cancelRead caches the source's close result for its owner to inspect. It may
// run concurrently with Read; sync.Once also orders concurrent Close callers.
func (s *ownedStream) cancelRead() {
	s.once.Do(func() { s.err = s.ReadCloser.Close() })
}
