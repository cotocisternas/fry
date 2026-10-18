//go:build linux

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"math/big"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/dbschema"
	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/installation"
	"github.com/cotocisternas/fry/internal/receiptstore"
	"github.com/cotocisternas/fry/internal/replicaconfig"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The same race-instrumented test executable runs the real command entry point.
// Flags, environment, exit status and signals cross an actual process boundary.
func TestMemberCommandProcess(_ *testing.T) {
	if os.Getenv("FRY_MEMBER_COMMAND_PROCESS") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{os.Args[0]}, os.Args[i+1:]...)
			break
		}
	}
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	main()
	os.Exit(0)
}

func memberCommandPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("FRY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL via scripts/tests/receipts.sh")
	}
	ctx := t.Context()
	control, err := pgxpool.New(ctx, dsn)
	checkCommand(t, err)
	t.Cleanup(control.Close)
	id, err := hotstore.NewID()
	checkCommand(t, err)
	name := "member_command_" + id.String()
	quoted := pgx.Identifier{name}.Sanitize()
	{
		_, err := control.Exec(ctx, "CREATE SCHEMA "+quoted)
		checkCommand(t, err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := control.Exec(cleanup, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	checkCommand(t, err)
	cfg.ConnConfig.RuntimeParams["search_path"] = name
	checkCommand(t, dbschema.ConfigurePool(cfg))
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	checkCommand(t, err)
	t.Cleanup(pool.Close)
	checkCommand(t, installation.Initialize(ctx, pool))
	return pool
}

type memberCommandTrust struct {
	files                replicaconfig.TLS
	client               *tls.Config
	server               *tls.Config
	clientKey, serverKey [32]byte
}

func memberTrust(t *testing.T) memberCommandTrust {
	t.Helper()
	caPublic, caKey, err := ed25519.GenerateKey(rand.Reader)
	checkCommand(t, err)
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "synthetic member command CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, template, template, caPublic, caKey)
	checkCommand(t, err)
	ca, err := x509.ParseCertificate(caDER)
	checkCommand(t, err)
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	root, err := os.OpenRoot(t.TempDir())
	checkCommand(t, err)
	defer func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	}()
	checkCommand(t, root.WriteFile("ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600))

	server := issueMemberCertificate(t, root, template, ca, caKey, "server", 2, x509.ExtKeyUsageServerAuth)
	client := issueMemberCertificate(t, root, template, ca, caKey, "client", 3, x509.ExtKeyUsageClientAuth)
	return memberCommandTrust{
		files: replicaconfig.TLS{
			CAFile:          filepath.Join(root.Name(), "ca.pem"),
			CertificateFile: filepath.Join(root.Name(), "server.pem"),
			KeyFile:         filepath.Join(root.Name(), "server-key.pem"),
		},
		client: &tls.Config{
			MinVersion:   tls.VersionTLS13,
			RootCAs:      roots,
			Certificates: []tls.Certificate{client},
		},
		server: &tls.Config{
			MinVersion:   tls.VersionTLS13,
			ClientCAs:    roots,
			Certificates: []tls.Certificate{server},
		},
		clientKey: sha256.Sum256(
			client.Leaf.RawSubjectPublicKeyInfo,
		),
		serverKey: sha256.Sum256(server.Leaf.RawSubjectPublicKeyInfo),
	}
}

func memberConfig(t *testing.T, trust memberCommandTrust) config {
	t.Helper()
	return config{
		ID:              receiptstore.ID{15: 1}.String(),
		Domain:          "zone-a",
		Listen:          "127.0.0.1:0",
		DataDirectory:   filepath.Join(t.TempDir(), "member"),
		MaxMessageBytes: 1 << 20,
		CapacityBytes:   16 << 20,
		MaxInFlight:     2,
		MaxRequests:     2,
		TimeoutSeconds:  5,
		TLS:             trust.files,
		CoordinatorKeys: []string{hex.EncodeToString(trust.clientKey[:])},
	}
}

type memberProcess struct {
	command *exec.Cmd
	done    chan struct{}
	err     error
	logPath string
}

func startMember(t *testing.T, pool *pgxpool.Pool, cfg config) *memberProcess {
	t.Helper()
	root, err := os.OpenRoot(t.TempDir())
	checkCommand(t, err)
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	})
	raw, err := json.Marshal(cfg)
	checkCommand(t, err)
	checkCommand(t, root.WriteFile("config.json", raw, 0o600))
	logs, err := root.Create("process.log")
	checkCommand(t, err)
	t.Cleanup(func() {
		if err := logs.Close(); err != nil {
			t.Error(err)
		}
	})
	executable, err := os.Executable()
	checkCommand(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	t.Cleanup(cancel)
	// G204,G702: this test executable and test-owned configuration, no shell; owner FRY maintainers,
	// review by 2027-01-18.
	//nolint:gosec // Reviewed call; rationale above.
	command := exec.CommandContext(
		ctx,
		executable,
		"-test.run=^TestMemberCommandProcess$",
		"--",
		"-config",
		filepath.Join(root.Name(), "config.json"),
	)
	dsn, err := url.Parse(pool.Config().ConnString())
	checkCommand(t, err)
	query := dsn.Query()
	query.Set("search_path", pool.Config().ConnConfig.RuntimeParams["search_path"])
	dsn.RawQuery = query.Encode()
	command.Env = append(os.Environ(), "FRY_MEMBER_COMMAND_PROCESS=1", "FRY_DATABASE_URL="+dsn.String())
	command.Stdout = logs
	command.Stderr = logs
	command.WaitDelay = 2 * time.Second
	checkCommand(t, command.Start())
	process := &memberProcess{
		command: command,
		done:    make(chan struct{}),
		logPath: filepath.Join(root.Name(), "process.log"),
	}
	go func() { process.err = command.Wait(); close(process.done) }()
	cleanupMemberProcess(t, command, process)
	return process
}

func (p *memberProcess) logs(t *testing.T) string {
	t.Helper()
	// logPath comes only from the test's temporary directory.
	raw, err := os.ReadFile(p.logPath)
	checkCommand(t, err)
	return string(raw)
}

func (p *memberProcess) wait(t *testing.T) error {
	t.Helper()
	select {
	case <-p.done:
		return p.err
	case <-time.After(20 * time.Second):
		t.Fatalf("member command did not exit within budget: %s", p.logs(t))
		return nil
	}
}

func (p *memberProcess) address(t *testing.T) string {
	t.Helper()
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		for line := range strings.SplitSeq(p.logs(t), "\n") {
			if _, address, ok := strings.Cut(line, "listening with mutual TLS on "); ok {
				return strings.TrimSpace(address)
			}
		}
		select {
		case <-p.done:
			t.Fatalf("member exited before serving: %v logs=%s", p.err, p.logs(t))
		case <-timeout.C:
			t.Fatalf("member did not start within budget: %s", p.logs(t))
		case <-tick.C:
		}
	}
}

func TestMemberCommandSchemaRefusalPrecedesStorageCreation(t *testing.T) {
	pool := memberCommandPool(t)
	cfg := memberConfig(t, memberTrust(t))
	{
		_, err := pool.Exec(t.Context(), `DROP TABLE fry_schema_version`)
		checkCommand(t, err)
	}
	process := startMember(t, pool, cfg)
	err := process.wait(t)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("schema refusal exit=%v logs=%s", err, process.logs(t))
	}
	if _, err := os.Stat(cfg.DataDirectory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("schema refusal created storage: %v", err)
	}
}

func TestMemberCommandStartsAndStopsOnSignal(t *testing.T) {
	pool := memberCommandPool(t)
	cfg := memberConfig(t, memberTrust(t))
	process := startMember(t, pool, cfg)
	if address := process.address(t); address == "" {
		t.Fatal("member did not publish its listening address")
	}
	checkCommand(t, process.command.Process.Signal(os.Interrupt))
	checkCommand(t, process.wait(t))
}

func issueMemberCertificate(
	t *testing.T,
	root *os.Root,
	template, ca *x509.Certificate,
	caKey ed25519.PrivateKey,
	name string,
	serial int64,
	usage x509.ExtKeyUsage,
) tls.Certificate {
	t.Helper()

	public, key, err := ed25519.GenerateKey(rand.Reader)
	checkCommand(t, err)
	cert := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		NotBefore:    template.NotBefore,
		NotAfter:     template.NotAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, ca, public, caKey)
	checkCommand(t, err)
	private, err := x509.MarshalPKCS8PrivateKey(key)
	checkCommand(t, err)
	checkCommand(
		t,
		root.WriteFile(name+".pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600),
	)
	checkCommand(
		t,
		root.WriteFile(name+"-key.pem", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}), 0o600),
	)
	leaf, err := x509.ParseCertificate(der)
	checkCommand(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

func cleanupMemberProcess(t *testing.T, command *exec.Cmd, process *memberProcess) {
	t.Helper()
	t.Cleanup(func() {
		select {
		case <-process.done:
			return
		default:
		}
		if err := command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Error(err)
		}
		select {
		case <-process.done:
		case <-time.After(5 * time.Second):
			t.Error("member process did not exit after forced cleanup")
		}
	})
}
