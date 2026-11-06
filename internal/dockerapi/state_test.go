package dockerapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fulcrumproject/agent-mysql/internal/config"
	"github.com/fulcrumproject/agent-mysql/internal/mysqlagent"
	"github.com/moby/moby/api/types/container"
	"golang.org/x/sys/unix"
)

const agentID = "11111111-1111-4111-8111-111111111111"
const serviceID = "22222222-2222-4222-8222-222222222222"

func stateClient(t *testing.T) (*Client, mysqlagent.Properties) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	c := &Client{agentID: agentID, cfg: config.DockerConfig{StateDir: dir, BindIP: "127.0.0.1", Host: "localhost"}.WithDefaults()}
	if err := privateDir(filepath.Join(dir, agentID)); err != nil {
		t.Fatal(err)
	}
	return c, mysqlagent.Properties{CPU: 1, Memory: 1024, Database: "app_data", Username: "appuser", Password: "long-'quoted'\\secret", Port: 33061}
}
func TestStateRetryAndImmutableCredentials(t *testing.T) {
	c, p := stateClient(t)
	if _, err := c.ensureState(serviceID, p, false); err != nil {
		t.Fatal(err)
	}
	rootFile := filepath.Join(c.dir(serviceID), "secrets", "root-password")
	before, err := os.ReadFile(rootFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.ensureState(serviceID, p, true); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(rootFile)
	if string(before) != string(after) {
		t.Fatal("root credential regenerated")
	}
	originalPassword := p.Password
	p.Password = "different-secret-for-test"
	if _, err = c.ensureState(serviceID, p, true); err == nil {
		t.Fatal("password mutation accepted")
	}
	if err = os.Remove(rootFile); err != nil {
		t.Fatal(err)
	}
	p.Password = originalPassword
	if _, err = c.ensureState(serviceID, p, true); err == nil {
		t.Fatal("missing root credentials silently accepted")
	}
	info, err := os.Stat(filepath.Join(c.dir(serviceID), "secrets", "client.cnf"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatal("client credentials are not private")
	}
}
func TestRestrictedUmaskStillAllowsMySQLToReadItsConfig(t *testing.T) {
	old := unix.Umask(0077)
	defer unix.Umask(old)
	c, p := stateClient(t)
	if _, err := c.ensureState(serviceID, p, false); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(c.dir(serviceID), "config"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0755 {
		t.Fatal("non-root mysqld cannot traverse its configuration directory")
	}
}

func TestMissingStateCannotAdoptExistingVolume(t *testing.T) {
	c, p := stateClient(t)
	if _, err := c.ensureState(serviceID, p, true); err == nil {
		t.Fatal("adopted resources without credentials")
	}
	if _, err := os.Stat(c.dir(serviceID)); !os.IsNotExist(err) {
		t.Fatal("created new state for existing volume")
	}
}
func TestCorruptStateAndSymlinkRejected(t *testing.T) {
	c, p := stateClient(t)
	target := t.TempDir()
	if err := os.Symlink(target, c.dir(serviceID)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ensureState(serviceID, p, false); err == nil {
		t.Fatal("followed service state symlink")
	}
}
func TestContainerConfigurationDoesNotExposePassword(t *testing.T) {
	c, p := stateClient(t)
	opts := c.createOptions(serviceID, p)
	b, err := json.Marshal(opts)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), p.Password) {
		t.Fatal("password leaked into container config")
	}
	if opts.Config.Image != config.Image || opts.HostConfig.RestartPolicy.Name != "unless-stopped" {
		t.Fatal("image or restart policy changed")
	}
	bind := opts.HostConfig.PortBindings[mysqlPort]
	if len(bind) != 1 || bind[0].HostIP.String() != "127.0.0.1" || bind[0].HostPort != "33061" {
		t.Fatal("unexpected published endpoint")
	}
	if !strings.Contains(strings.Join(opts.Config.Env, " "), "MYSQL_ROOT_HOST=localhost") {
		t.Fatal("remote root would be enabled by image default")
	}
	if err = c.owned(c.labels(serviceID), serviceID); err != nil {
		t.Fatal(err)
	}
	if c.owned(c.labels(serviceID), agentID) == nil {
		t.Fatal("foreign resource accepted")
	}
}
func TestOnlyOneProcessMayOwnAgentState(t *testing.T) {
	c, _ := stateClient(t)
	first, err := NewClient(c.cfg, c.agentID)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate, err := NewClient(c.cfg, c.agentID); err == nil {
		duplicate.Close()
		first.Close()
		t.Fatal("duplicate agent process accepted")
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := NewClient(c.cfg, c.agentID)
	if err != nil {
		t.Fatal(err)
	}
	next.Close()
}

func TestUsageNormalization(t *testing.T) {
	s := container.StatsResponse{}
	s.CPUStats.CPUUsage.TotalUsage = 150
	s.PreCPUStats.CPUUsage.TotalUsage = 100
	s.CPUStats.SystemUsage = 200
	s.PreCPUStats.SystemUsage = 100
	s.CPUStats.OnlineCPUs = 4
	s.MemoryStats.Usage = 512
	cpu, mem := usage(s, 2e9, 1024)
	if cpu != 1 || mem != 0.5 {
		t.Fatalf("got %v, %v", cpu, mem)
	}
	s.CPUStats.CPUUsage.TotalUsage = 0
	cpu, _ = usage(s, 2e9, 1024)
	if cpu != 0 {
		t.Fatal("counter reset produced usage underflow")
	}
}
