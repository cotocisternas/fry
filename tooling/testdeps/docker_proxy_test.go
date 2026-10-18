package testdeps_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	deps "github.com/cotocisternas/fry/tooling/testdeps"

	"github.com/moby/moby/client"
	"github.com/testcontainers/testcontainers-go"
)

type daemonProbe struct {
	missing    bool
	rejectRyuk bool
	stallLogs  bool
	mu         sync.Mutex
	pulls      int
	logs       int
	deletes    int
	created    []string
}

func TestRuntimeUsesOnlyBuiltImagesAndNeverPulls(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	probe := &daemonProbe{}
	environment := probe.serve(t)
	running := startCLI(t, ctx, environment...)
	docker, images := checkedDocker(t, ctx)
	for _, id := range probe.containers() {
		actual, err := docker.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if actual.Container.Image != images["postgres"] && actual.Container.Image != images["ryuk"] {
			t.Fatalf("runtime used unowned image %s", actual.Container.Image)
		}
	}
	if len(probe.containers()) != 2 {
		t.Fatalf("created %d containers, want PostgreSQL and locally built Ryuk", len(probe.containers()))
	}
	running.close(t, ctx)
	if probe.pullCount() != 0 {
		t.Fatalf("runtime attempted %d image pulls", probe.pullCount())
	}
}

func TestMissingLocalImageFailsBeforeCreatingOrPulling(t *testing.T) {
	probe := &daemonProbe{missing: true}
	environment := probe.serve(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "../../scripts/test-deps.sh", "run", "--", "true")
	command.Env = append(os.Environ(), environment...)
	output, err := command.CombinedOutput()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 1 {
		t.Fatalf("missing-image status: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "run scripts/test-deps.sh build") {
		t.Fatalf("missing setup instruction: %s", output)
	}
	if probe.pullCount() != 0 || len(probe.containers()) != 0 {
		t.Fatal("missing local image caused a pull or container creation")
	}
}

func (p *daemonProbe) serve(t *testing.T) []string {
	t.Helper()
	socket := "/var/run/docker.sock"
	if host := os.Getenv("DOCKER_HOST"); strings.HasPrefix(host, "unix://") {
		socket = strings.TrimPrefix(host, "unix://")
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	proxy := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: "http", Host: "docker"})
	proxy.Transport = transport
	proxy.ModifyResponse = p.captureCreated
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p.handleCleanupTraffic(r) {
			return
		}
		if p.rejectRyuk && strings.HasSuffix(r.URL.Path, "/containers/create") &&
			strings.HasPrefix(r.URL.Query().Get("name"), "reaper_") {
			http.Error(w, "deliberate Ryuk creation failure", http.StatusBadRequest)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/images/create") {
			p.mu.Lock()
			p.pulls++
			p.mu.Unlock()
			http.Error(w, "test forbids image pulls", http.StatusForbidden)
			return
		}
		if p.missing && strings.Contains(r.URL.Path, "/images/fry-testdeps-postgres:") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			if _, err := io.WriteString(w, `{"message":"fixture image deliberately unavailable"}`); err != nil {
				t.Error(err)
			}
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return []string{
		"DOCKER_HOST=" + strings.Replace(server.URL, "http://", "tcp://", 1),
		"DOCKER_TLS_VERIFY=",
		"DOCKER_CERT_PATH=",
		"TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=" + socket,
		"TESTCONTAINERS_HOST_OVERRIDE=127.0.0.1",
	}
}

func (p *daemonProbe) captureCreated(response *http.Response) error {
	if !strings.HasSuffix(response.Request.URL.Path, "/containers/create") {
		return nil
	}
	data, err := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if err != nil || closeErr != nil {
		return errors.Join(err, closeErr)
	}
	response.Body = io.NopCloser(bytes.NewReader(data))
	var value struct{ ID string }
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	if value.ID != "" {
		p.mu.Lock()
		p.created = append(p.created, value.ID)
		p.mu.Unlock()
	}
	return nil
}

func (p *daemonProbe) containers() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string{}, p.created...)
}

func (p *daemonProbe) pullCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pulls
}

func (p *daemonProbe) cleanupTraffic() (logs, deletes int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.logs, p.deletes
}

func (p *daemonProbe) handleCleanupTraffic(r *http.Request) bool {
	if p.stallLogs && p.isPostgresLogs(r) {
		p.mu.Lock()
		p.logs++
		p.mu.Unlock()
		<-r.Context().Done()
		return true
	}
	if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/containers/") {
		p.mu.Lock()
		p.deletes++
		p.mu.Unlock()
	}
	return false
}

func (p *daemonProbe) isPostgresLogs(r *http.Request) bool {
	created := p.containers()
	return len(created) > 0 && strings.HasSuffix(r.URL.Path, "/containers/"+created[0]+"/logs")
}

func checkedDocker(t *testing.T, ctx context.Context) (*testcontainers.DockerClient, map[string]string) {
	t.Helper()
	docker, err := testcontainers.NewDockerClientWithOpts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := docker.Close(); err != nil {
			t.Error(err)
		}
	})
	images, err := deps.Check(ctx, docker)
	if err != nil {
		t.Fatal(err)
	}
	return docker, images
}
