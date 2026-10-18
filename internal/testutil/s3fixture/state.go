package s3fixture

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	maxStateBytes     = 64 << 20
	maxStateBodyBytes = 32 << 20
	maxStateObjects   = 128
)

// State persistence is deliberately bounded to small synthetic rehearsals. It
// preserves the fixture's current map, not S3 versions, IAM or AWS durability.
type snapshotState struct {
	Version  int              `json:"version"`
	Endpoint string           `json:"endpoint"`
	Config   *Config          `json:"config"`
	Serial   int64            `json:"serial"`
	Objects  []snapshotObject `json:"objects"`
}
type snapshotObject struct {
	Key     string `json:"key"`
	Body    []byte `json:"body"`
	Digest  string `json:"digest"`
	ETag    string `json:"etag"`
	Retired bool   `json:"retired"`
}

func loopbackAddress(address string, allowZero bool) (string, error) {
	host, rawPort, err := net.SplitHostPort(address)
	if err != nil {
		return "", errors.New("s3fixture: numeric loopback listen address required")
	}
	ip := net.ParseIP(host)
	port, e := strconv.Atoi(rawPort)
	if ip == nil || !ip.IsLoopback() || e != nil || port < 0 || port > 65535 || port == 0 && !allowZero {
		return "", errors.New("s3fixture: numeric loopback listen address required")
	}
	canonical := net.JoinHostPort(ip.String(), strconv.Itoa(port))
	if canonical != address {
		return "", errors.New("s3fixture: canonical numeric loopback address required")
	}
	return canonical, nil
}

func newListeningFixture(
	config Config,
	listener net.Listener,
	objects map[string]Object,
	serial int,
) (*Fixture, error) {
	f := &Fixture{objects: objects, config: config, serial: serial}
	server := httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	// httptest creates a disposable listener eagerly. Replace it before Start;
	// never retry another address if the requested retained endpoint cannot bind.
	if err := server.Listener.Close(); err != nil {
		return nil, errors.Join(err, listener.Close())
	}
	server.Listener = listener
	f.Server = server
	server.Start()
	return f, nil
}

// NewAt starts the same narrow HTTP fixture at a numeric loopback address. A
// source may request port zero; exported state records the actual assigned port.
func NewAt(config Config, listenAddr string) (*Fixture, error) {
	return NewAtContext(context.Background(), config, listenAddr)
}

// NewAtContext starts a fixture with cancelable listener setup.
func NewAtContext(ctx context.Context, config Config, listenAddr string) (*Fixture, error) {
	address, err := loopbackAddress(listenAddr, true)
	if err != nil {
		return nil, err
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	return newListeningFixture(config, listener, map[string]Object{}, 0)
}

func validateSnapshot(state snapshotState) (map[string]Object, error) {
	if err := state.validateHeader(); err != nil {
		return nil, err
	}
	objects := make(map[string]Object, len(state.Objects))
	etags := make(map[string]bool, len(state.Objects))
	total := 0
	for _, entry := range state.Objects {
		if !validSnapshotKey(entry.Key) || entry.Body == nil || len(entry.Body) > maxStateBodyBytes ||
			len(entry.Body) > maxStateBodyBytes-total {
			return nil, errors.New("s3fixture: invalid snapshot object or body limits")
		}
		total += len(entry.Body)
		if _, exists := objects[entry.Key]; exists {
			return nil, errors.New("s3fixture: duplicate snapshot object key")
		}
		digest, err := entry.validateIdentity(state.Serial, etags)
		if err != nil {
			return nil, err
		}

		etags[entry.ETag] = true
		objects[entry.Key] = Object{
			Body:    entry.Body,
			Digest:  digest,
			ETag:    entry.ETag,
			Retired: entry.Retired,
		}
	}
	return objects, nil
}

func (state snapshotState) validateHeader() error {
	if state.Version != 1 || state.Config == nil || state.Objects == nil || len(state.Objects) > maxStateObjects ||
		!validStateSerial(state.Serial) {
		return errors.New("s3fixture: invalid snapshot header or limits")
	}
	address := strings.TrimPrefix(state.Endpoint, "http://")
	canonical, err := loopbackAddress(address, false)
	if err != nil || state.Endpoint != "http://"+canonical {
		return errors.New("s3fixture: invalid snapshot endpoint")
	}
	if len(state.Config.Versioning) > 32 || !utf8.ValidString(state.Config.Versioning) {
		return errors.New("s3fixture: invalid snapshot configuration")
	}
	return nil
}

func (entry snapshotObject) validateIdentity(maxSerial int64, etags map[string]bool) ([32]byte, error) {
	digest := sha256.Sum256(entry.Body)
	if entry.Digest != hex.EncodeToString(digest[:]) || entry.Retired && len(entry.Body) != 0 {
		return digest, errors.New("s3fixture: invalid object digest or retirement marker")
	}
	serial, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(entry.ETag, "\"opaque-"), "\""), 10, 64)
	if err != nil || serial < 1 || serial > maxSerial || entry.ETag != fmt.Sprintf("\"opaque-%d\"", serial) ||
		etags[entry.ETag] {
		return digest, errors.New("s3fixture: invalid snapshot ETag or serial")
	}
	return digest, nil
}

func validSnapshotKey(key string) bool {
	if key == "" || len(key) > 1024 || !utf8.ValidString(key) || strings.HasPrefix(key, "/") || key == "." ||
		path.Clean(key) != key ||
		key == ".." ||
		strings.HasPrefix(key, "../") {
		return false
	}
	return printableSnapshotKey(key)
}

func printableSnapshotKey(key string) bool {
	for _, r := range key {
		if r < 32 || r == 127 {
			return false
		}
	}
	return true
}

// RestoreAt validates the entire archive before binding. The saved endpoint is
// immutable: there is no random-port fallback, object recreation or ETag change.
func RestoreAt(statePath, listenAddr string) (*Fixture, error) {
	return RestoreAtContext(context.Background(), statePath, listenAddr)
}

// RestoreAtContext validates the archive and uses ctx for listener setup.
func RestoreAtContext(ctx context.Context, statePath, listenAddr string) (*Fixture, error) {
	address, err := loopbackAddress(listenAddr, false)
	if err != nil {
		return nil, err
	}
	data, err := readSnapshot(statePath)
	if err != nil {
		return nil, err
	}
	var state snapshotState
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&state); err != nil {
		return nil, err
	}
	// Only exact EOF proves the JSON document ended without trailing data.
	if decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("s3fixture: trailing snapshot data")
	}
	objects, err := validateSnapshot(state)
	if err != nil {
		return nil, err
	}
	if state.Endpoint != "http://"+address {
		return nil, errors.New("s3fixture: restore endpoint differs from snapshot")
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	return newListeningFixture(*state.Config, listener, objects, int(state.Serial))
}

func readSnapshot(statePath string) ([]byte, error) {
	info, err := os.Lstat(statePath)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxStateBytes {
		return nil, errors.New("s3fixture: snapshot must be a bounded regular file")
	}
	// trusted operator-selected synthetic snapshot path, bounded and same-file checked on restore;
	// owner: FRY maintainers; review: 2027-01-18
	// #nosec G304 -- Reviewed call; rationale above.
	file, err := os.Open(
		statePath,
	)
	if err != nil {
		return nil, err
	}
	opened, e := file.Stat()
	if e != nil {
		return nil, errors.Join(e, file.Close())
	}
	if !os.SameFile(info, opened) {
		return nil, errors.Join(errors.New("s3fixture: snapshot changed while opening"), file.Close())
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxStateBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	if len(data) > maxStateBytes {
		return nil, errors.New("s3fixture: snapshot too large")
	}
	return data, nil
}

// SaveState exports only after HTTP Close has completed. Request logs and the
// transient unavailability switch are intentionally excluded. Existing archives
// are never overwritten; a failed publication never becomes a valid snapshot.
func (f *Fixture) SaveState(statePath string) (retErr error) {
	data, err := f.encodeSnapshot()
	if err != nil {
		return err
	}
	parent := filepath.Dir(statePath)
	temp, err := os.CreateTemp(parent, ".s3fixture-state-")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer func() {
		if err := os.Remove(tempName); err != nil && !errors.Is(err, os.ErrNotExist) {
			retErr = errors.Join(retErr, err)
		}
	}()
	if err = temp.Chmod(0o600); err != nil {
		return errors.Join(err, temp.Close())
	}
	_, writeErr := temp.Write(data)
	syncErr := temp.Sync()
	closeErr := temp.Close()
	if err = errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	// Link supplies atomic no-replace publication within the same directory.
	if err = os.Link(tempName, statePath); err != nil {
		return err
	}
	if err = os.Remove(tempName); err != nil {
		return err
	}
	// trusted synthetic snapshot directory opened only to fsync publication; owner: FRY maintainers;
	// review: 2027-01-18
	// #nosec G304 -- Reviewed call; rationale above.
	directory, err := os.Open(
		parent,
	)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

func (f *Fixture) encodeSnapshot() ([]byte, error) {
	f.mu.Lock()
	if !f.closed {
		f.mu.Unlock()
		return nil, errors.New("s3fixture: close HTTP before exporting state")
	}
	if len(f.objects) > maxStateObjects {
		f.mu.Unlock()
		return nil, errors.New("s3fixture: too many objects for snapshot")
	}
	if err := snapshotBodyBudget(f.objects); err != nil {
		f.mu.Unlock()
		return nil, err
	}
	config := f.config
	state := snapshotState{
		Version:  1,
		Endpoint: f.Server.URL,
		Config:   &config,
		Serial:   int64(f.serial),
		Objects:  make([]snapshotObject, 0, len(f.objects)),
	}
	keys := make([]string, 0, len(f.objects))
	for key := range f.objects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		object := f.objects[key]
		body := object.Body
		if body == nil {
			body = []byte{}
		}
		state.Objects = append(
			state.Objects,
			snapshotObject{
				key,
				body,
				hex.EncodeToString(object.Digest[:]),
				object.ETag,
				object.Retired,
			},
		)
	}
	if _, err := validateSnapshot(state); err != nil {
		f.mu.Unlock()
		return nil, err
	}
	data, err := json.Marshal(state)
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if len(data) > maxStateBytes {
		return nil, errors.New("s3fixture: encoded snapshot too large")
	}
	return data, nil
}

func validStateSerial(serial int64) bool { return serial >= 0 && uint64(serial) <= uint64(^uint(0)>>1) }

func snapshotBodyBudget(objects map[string]Object) error {
	total := 0
	for _, object := range objects {
		if len(object.Body) > maxStateBodyBytes-total {
			return errors.New("s3fixture: snapshot body budget exceeded")
		}
		total += len(object.Body)
	}
	return nil
}
