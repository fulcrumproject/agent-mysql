//go:build integration

package dockerapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/fulcrumproject/agent-mysql/internal/config"
	"github.com/fulcrumproject/agent-mysql/internal/mysqlagent"
	"github.com/moby/moby/client"
)

func uniqueID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}
func port(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestDockerLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	stateDir := t.TempDir()
	must(t, os.Chmod(stateDir, 0700))
	aid := uniqueID()
	id := uniqueID()
	cfg := config.DockerConfig{StateDir: stateDir, BindIP: "127.0.0.1", Host: "127.0.0.1"}.WithDefaults()
	backend, err := NewClient(cfg, aid)
	must(t, err)
	c := backend.(*Client)
	must(t, c.Ping(ctx))
	passwordBytes := make([]byte, 16)
	_, err = rand.Read(passwordBytes)
	must(t, err)
	p := mysqlagent.Properties{CPU: 1, Memory: 1024, Database: "customer_data", Username: "customer", Password: hex.EncodeToString(passwordBytes) + "'\\\"# $", Port: port(t)}
	t.Cleanup(func() {
		defer func() { must(t, c.Close()) }()
		cleanup, done := context.WithTimeout(context.Background(), 60*time.Second)
		defer done()
		_, _ = c.Stop(cleanup, id)
		if err := c.Delete(cleanup, id); err != nil {
			t.Errorf("cleanup service %s: %v", id, err)
		}
	})
	// A bind conflict must fail without deleting data or generating another root key.
	occupied, err := net.Listen("tcp", net.JoinHostPort(cfg.BindIP, strconv.Itoa(p.Port)))
	must(t, err)
	_, createErr := c.Create(ctx, id, p)
	must(t, occupied.Close())
	if createErr == nil {
		t.Fatal("expected bind failure")
	}
	first, err := c.inspect(ctx, id)
	must(t, err)
	if first == nil {
		t.Fatal("partial create discarded its container")
	}
	rootBefore, err := os.ReadFile(filepath.Join(c.dir(id), "secrets/root-password"))
	must(t, err)
	t.Log("bind failure retained owned resources")
	instance, err := c.Create(ctx, id, p)
	must(t, err)
	if instance.ContainerID != first.ID {
		t.Fatal("retry duplicated container")
	}
	same, err := c.Create(ctx, id, p)
	must(t, err)
	if same.ContainerID != instance.ContainerID {
		t.Fatal("non-idempotent create")
	}
	rootAfter, err := os.ReadFile(filepath.Join(c.dir(id), "secrets/root-password"))
	must(t, err)
	if !bytes.Equal(rootBefore, rootAfter) {
		t.Fatal("retry regenerated root password")
	}
	inspect, err := c.api.ContainerInspect(ctx, instance.ContainerID, client.ContainerInspectOptions{})
	must(t, err)
	raw, err := json.Marshal(inspect)
	must(t, err)
	if bytes.Contains(raw, []byte(p.Password)) || bytes.Contains(raw, rootAfter) {
		t.Fatal("credentials exposed in Docker inspect")
	}
	logs, err := c.api.ContainerLogs(ctx, instance.ContainerID, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
	must(t, err)
	logData, err := io.ReadAll(logs)
	must(t, err)
	logs.Close()
	if bytes.Contains(logData, []byte(p.Password)) || bytes.Contains(logData, rootAfter) {
		t.Fatal("credentials exposed in MySQL logs")
	}
	t.Log("create retry reused container and credentials; inspect and logs have no secrets")
	out, err := externalSQL(ctx, c, id, instance.Port, "client", "CREATE TABLE evidence (id INT PRIMARY KEY, value VARCHAR(40)); INSERT INTO evidence VALUES (1,'persisted'); SELECT value FROM evidence;")
	must(t, err)
	if !strings.Contains(out, "persisted") {
		t.Fatal("SQL write/read failed")
	}
	for _, query := range []string{"SELECT authentication_string FROM mysql.user", "CREATE DATABASE forbidden", "CREATE DATABASE customerXdata"} {
		if _, err = externalSQL(ctx, c, id, instance.Port, "client", query); err == nil {
			t.Fatalf("privilege escape: %s", query)
		}
	}
	if _, err = externalSQL(ctx, c, id, instance.Port, "admin", "SELECT 1"); err == nil {
		t.Fatal("root is remotely accessible")
	}
	if c.Delete(ctx, id) == nil {
		t.Fatal("deleted running database")
	}
	if _, err = c.Update(ctx, id, 2, 1536); err == nil {
		t.Fatal("updated running database")
	}
	t.Log("published private endpoint works; database isolation and remote-root denial verified")
	_, err = c.Stop(ctx, id)
	must(t, err)
	_, err = c.Update(ctx, id, 2, 1536)
	must(t, err)
	_, err = c.Update(ctx, id, 2, 1536)
	must(t, err)
	inspect, err = c.api.ContainerInspect(ctx, instance.ContainerID, client.ContainerInspectOptions{})
	must(t, err)
	if inspect.Container.State.Running || inspect.Container.HostConfig.Memory != 1536*1024*1024 || inspect.Container.HostConfig.NanoCPUs != 2e9 || inspect.Container.HostConfig.RestartPolicy.Name != "unless-stopped" {
		t.Fatal("cold resource update not applied")
	}
	// Reopening the backend models a new agent process, not a Docker daemon restart.
	must(t, c.Close())
	backend, err = NewClient(cfg, aid)
	must(t, err)
	c = backend.(*Client)
	running, err := c.inspect(ctx, id)
	must(t, err)
	if active(running) {
		t.Fatal("agent restart started a stopped database")
	}
	again, err := c.Start(ctx, id)
	must(t, err)
	if again.Port != instance.Port || again.ContainerID != instance.ContainerID {
		t.Fatal("endpoint or identity changed")
	}
	out, err = externalSQL(ctx, c, id, instance.Port, "client", "SELECT value FROM evidence; SELECT @@innodb_buffer_pool_size;")
	must(t, err)
	if !strings.Contains(out, "persisted") || !strings.Contains(out, strconv.Itoa(768*1024*1024)) {
		t.Fatal("data or cold configuration lost")
	}
	t.Log("stop/update/agent restart/start preserved data and endpoint, and applied MySQL sizing")
	// Server-initiated exit exercises restart policy; Docker kill would count as a manual stop.
	beforeExit, err := c.api.ContainerInspect(ctx, instance.ContainerID, client.ContainerInspectOptions{})
	must(t, err)
	must(t, c.sql(ctx, instance.ContainerID, "admin", "SHUTDOWN", &bytes.Buffer{}))
	restarted := false
	for deadline := time.Now().Add(45 * time.Second); time.Now().Before(deadline); {
		current, inspectErr := c.api.ContainerInspect(ctx, instance.ContainerID, client.ContainerInspectOptions{})
		if inspectErr == nil && current.Container.RestartCount > beforeExit.Container.RestartCount && current.Container.State.Running {
			restarted = true
			break
		}
		time.Sleep(time.Second)
	}
	if !restarted {
		t.Fatal("Docker did not restart the exited server")
	}
	readyCtx, readyCancel := context.WithTimeout(ctx, 45*time.Second)
	defer readyCancel()
	must(t, c.waitReady(readyCtx, instance.ContainerID))
	out, err = externalSQL(ctx, c, id, instance.Port, "client", "SELECT value FROM evidence;")
	must(t, err)
	if !strings.Contains(out, "persisted") {
		t.Fatal("data missing after server restart")
	}
	m, err := c.Metrics(ctx, id)
	must(t, err)
	if m.Available != 1 || m.StorageFreeBytes <= 0 || m.Memory <= 0 {
		t.Fatalf("unexpected metrics: %+v", m)
	}
	t.Log("automatic server restart and SQL availability/resource/disk metrics verified")
	// Two instances may use the same database/user names but must never share credentials or data.
	secondID := uniqueID()
	secondProps := p
	secondProps.Port = port(t)
	secondProps.Password = hex.EncodeToString(passwordBytes) + "-second"
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 60*time.Second)
		defer done()
		_, _ = c.Stop(cleanup, secondID)
		if err := c.Delete(cleanup, secondID); err != nil {
			t.Errorf("cleanup second service: %v", err)
		}
	})
	second, err := c.Create(ctx, secondID, secondProps)
	must(t, err)
	if second.Volume == instance.Volume || second.ContainerID == instance.ContainerID {
		t.Fatal("services share resources")
	}
	if _, err = externalSQL(ctx, c, id, second.Port, "client", "SELECT 1"); err == nil {
		t.Fatal("first credentials authorized against second service")
	}
	if _, err = externalSQL(ctx, c, secondID, instance.Port, "client", "SELECT 1"); err == nil {
		t.Fatal("second credentials authorized against first service")
	}
	_, err = externalSQL(ctx, c, secondID, second.Port, "client", "CREATE TABLE evidence (value VARCHAR(40)); INSERT INTO evidence VALUES ('isolated');")
	must(t, err)
	out, err = externalSQL(ctx, c, id, instance.Port, "client", "SELECT value FROM evidence;")
	must(t, err)
	if !strings.Contains(out, "persisted") || strings.Contains(out, "isolated") {
		t.Fatal("service data isolation failed")
	}
	_, err = c.Stop(ctx, secondID)
	must(t, err)
	must(t, c.Delete(ctx, secondID))
	t.Log("two live services have separate data, credentials, containers and volumes")
	// Foreign volume with the expected name must not be adopted or removed.
	foreignID := uniqueID()
	foreignName := resourceName(foreignID)
	_, err = c.api.VolumeCreate(ctx, client.VolumeCreateOptions{Name: foreignName, Labels: map[string]string{"test": aid}})
	must(t, err)
	defer func() { _, _ = c.api.VolumeRemove(context.Background(), foreignName, client.VolumeRemoveOptions{}) }()
	if _, err = c.Create(ctx, foreignID, p); err == nil {
		t.Fatal("adopted foreign volume")
	}
	if err = c.Delete(ctx, foreignID); err == nil {
		t.Fatal("deleted foreign volume")
	}
	_, err = c.api.VolumeInspect(ctx, foreignName, client.VolumeInspectOptions{})
	must(t, err)
	_, err = c.Stop(ctx, id)
	must(t, err)
	must(t, c.Delete(ctx, id))
	must(t, c.Delete(ctx, id))
	_, err = c.api.VolumeInspect(ctx, instance.Volume, client.VolumeInspectOptions{})
	if !errdefs.IsNotFound(err) {
		t.Fatal("volume not removed")
	}
	if _, err = os.Stat(c.dir(id)); !os.IsNotExist(err) {
		t.Fatal("secret state not removed")
	}
	t.Log("delete is repeatable and removed only owned stopped service resources")
}

func externalSQL(ctx context.Context, c *Client, id string, port int, account, query string) (string, error) {
	// A short-lived native client on the host network exercises the published TCP port.
	name := "fulcrum-mysql-probe-" + uniqueID()
	args := []string{"--host", "unix://" + c.cfg.SocketPath, "run", "--rm", "--name", name, "--label", "io.fulcrum.mysql.test=" + id, "--network", "host",
		"--mount", "type=bind,source=" + filepath.Join(c.dir(id), "secrets") + ",target=/run/fulcrum-secrets,readonly",
		"--entrypoint", "mysql", config.Image, "--defaults-extra-file=/run/fulcrum-secrets/" + account + ".cnf", "--protocol=tcp", "--host=127.0.0.1",
		"--port=" + strconv.Itoa(port), "--ssl-mode=DISABLED", "--get-server-public-key", "--batch", "--skip-column-names", "--execute", query}
	defer exec.Command("docker", "--host", "unix://"+c.cfg.SocketPath, "rm", "-f", name).Run()
	output, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		safe := string(output)
		for _, filename := range []string{"password", "bootstrap-password", "root-password"} {
			if secret, readErr := os.ReadFile(filepath.Join(c.dir(id), "secrets", filename)); readErr == nil && len(secret) > 0 {
				safe = strings.ReplaceAll(safe, string(secret), "[REDACTED]")
			}
		}
		return "", fmt.Errorf("SQL client failed: %w: %s", err, safe)
	}
	return string(output), nil
}
