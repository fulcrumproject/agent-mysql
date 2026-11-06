package dockerapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/containerd/errdefs"
	"github.com/fulcrumproject/agent-mysql/internal/config"
	"github.com/fulcrumproject/agent-mysql/internal/mysqlagent"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"golang.org/x/sys/unix"
)

const agentLabel = "io.fulcrum.agent"
const serviceLabel = "io.fulcrum.service"
const managedLabel = "io.fulcrum.mysql"

var mysqlPort = network.MustParsePort("3306/tcp")

type Client struct {
	api     *client.Client
	cfg     config.DockerConfig
	agentID string
	lock    *os.File
	mu      sync.Mutex
}

var _ mysqlagent.DockerClient = (*Client)(nil)

func NewClient(cfg config.DockerConfig, agentID string) (mysqlagent.DockerClient, error) {
	cfg = cfg.WithDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if !mysqlagent.ValidID(agentID) {
		return nil, fmt.Errorf("invalid agent ID")
	}
	if err := privateDir(cfg.StateDir); err != nil {
		return nil, err
	}
	dir := filepath.Join(cfg.StateDir, agentID)
	if err := privateDir(dir); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "agent.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another agent process owns this state directory")
	}
	// Never read DOCKER_HOST or Docker contexts: this agent is local-only.
	api, err := client.New(client.WithHost("unix://" + cfg.SocketPath))
	if err != nil {
		f.Close()
		return nil, err
	}
	return &Client{api: api, cfg: cfg, agentID: agentID, lock: f}, nil
}

func (c *Client) Close() error { return errors.Join(c.api.Close(), c.lock.Close()) }
func (c *Client) Ping(ctx context.Context) error {
	if _, err := c.api.Ping(ctx, client.PingOptions{}); err != nil {
		return err
	}
	info, err := c.api.Info(ctx, client.InfoOptions{})
	if err != nil {
		return err
	}
	if info.Info.OSType != "linux" || !info.Info.MemoryLimit || !info.Info.CPUCfsQuota || !info.Info.SwapLimit || !info.Info.PidsLimit {
		return fmt.Errorf("Docker must run on Linux with memory, swap, CPU quota and PID limits enabled")
	}
	return nil
}
func resourceName(id string) string { return "fulcrum-mysql-" + id }
func (c *Client) labels(id string) map[string]string {
	return map[string]string{managedLabel: "true", agentLabel: c.agentID, serviceLabel: id}
}
func (c *Client) owned(labels map[string]string, id string) error {
	if labels[managedLabel] != "true" || labels[agentLabel] != c.agentID || labels[serviceLabel] != id {
		return fmt.Errorf("resource ownership mismatch for service %s", id)
	}
	return nil
}
func validateID(id string) error {
	if !mysqlagent.ValidID(id) {
		return fmt.Errorf("invalid service ID")
	}
	return nil
}

func (c *Client) inspect(ctx context.Context, id string) (*container.InspectResponse, error) {
	v, err := c.api.ContainerInspect(ctx, resourceName(id), client.ContainerInspectOptions{})
	if errdefs.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if v.Container.Config == nil || v.Container.State == nil || v.Container.HostConfig == nil {
		return nil, fmt.Errorf("incomplete Docker inspection")
	}
	if err = c.owned(v.Container.Config.Labels, id); err != nil {
		return nil, err
	}
	return &v.Container, nil
}

// preflight checks every existing resource before creating, updating or deleting any.
func (c *Client) preflight(ctx context.Context, id string) (bool, error) {
	v, err := c.inspect(ctx, id)
	if err != nil {
		return false, err
	}
	exists := v != nil
	vol, err := c.api.VolumeInspect(ctx, resourceName(id), client.VolumeInspectOptions{})
	if err == nil {
		exists = true
		if err = c.owned(vol.Volume.Labels, id); err != nil {
			return false, err
		}
	} else if !errdefs.IsNotFound(err) {
		return false, err
	}
	net, err := c.api.NetworkInspect(ctx, resourceName(id), client.NetworkInspectOptions{})
	if err == nil {
		exists = true
		if err = c.owned(net.Network.Labels, id); err != nil {
			return false, err
		}
	} else if !errdefs.IsNotFound(err) {
		return false, err
	}
	return exists, nil
}

func (c *Client) ensureImage(ctx context.Context) error {
	if _, err := c.api.ImageInspect(ctx, config.Image); err == nil {
		return nil
	} else if !errdefs.IsNotFound(err) {
		return err
	}
	pull, err := c.api.ImagePull(ctx, config.Image, client.ImagePullOptions{})
	if err != nil {
		return err
	}
	defer pull.Close()
	return pull.Wait(ctx)
}

func (c *Client) Create(ctx context.Context, id string, p mysqlagent.Properties) (*mysqlagent.Instance, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := validateID(id); err != nil {
		return nil, err
	}
	if err := p.Validate(c.cfg); err != nil {
		return nil, err
	}
	exists, err := c.preflight(ctx, id)
	if err != nil {
		return nil, err
	}
	s, err := c.ensureState(id, p, exists)
	if err != nil {
		return nil, err
	}
	if err = c.ensureImage(ctx); err != nil {
		return nil, err
	}
	name := resourceName(id)
	if _, err = c.api.VolumeInspect(ctx, name, client.VolumeInspectOptions{}); errdefs.IsNotFound(err) {
		if _, err = c.api.VolumeCreate(ctx, client.VolumeCreateOptions{Name: name, Driver: "local", Labels: c.labels(id)}); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if _, err = c.api.NetworkInspect(ctx, name, client.NetworkInspectOptions{}); errdefs.IsNotFound(err) {
		if _, err = c.api.NetworkCreate(ctx, name, client.NetworkCreateOptions{Driver: "bridge", Labels: c.labels(id)}); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	v, err := c.inspect(ctx, id)
	if err != nil {
		return nil, err
	}
	if v == nil {
		opts := c.createOptions(id, p)
		created, createErr := c.api.ContainerCreate(ctx, opts)
		if createErr != nil {
			return nil, createErr
		}
		if len(created.Warnings) > 0 {
			return nil, fmt.Errorf("Docker provisioning warnings; resources retained: %v", created.Warnings)
		}
		v, err = c.inspect(ctx, id)
		if err != nil {
			return nil, err
		}
	}
	if err = c.checkLayout(v, s); err != nil {
		return nil, err
	}
	if v.State.Running && (v.HostConfig.NanoCPUs != int64(p.CPU)*1e9 || v.HostConfig.Memory != p.Memory*1024*1024) {
		return nil, fmt.Errorf("running container limits differ; stop before updating resources")
	}
	if !v.State.Running {
		if err = writeMySQLConfig(filepath.Join(c.dir(id), "config"), p.Memory); err != nil {
			return nil, err
		}
		if err = c.updateResources(ctx, v.ID, p.CPU, p.Memory); err != nil {
			return nil, err
		}
	}
	return c.start(ctx, id, s, v)
}

func (c *Client) createOptions(id string, p mysqlagent.Properties) client.ContainerCreateOptions {
	name := resourceName(id)
	pids := int64(512)
	return client.ContainerCreateOptions{Name: name,
		NetworkingConfig: &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{name: {}}},
		Config: &container.Config{Image: config.Image, Labels: c.labels(id),
			Env: []string{"MYSQL_DATABASE=" + p.Database, "MYSQL_USER=" + p.Username, "MYSQL_ROOT_HOST=localhost",
				"MYSQL_PASSWORD_FILE=/run/fulcrum-secrets/bootstrap-password", "MYSQL_ROOT_PASSWORD_FILE=/run/fulcrum-secrets/root-password"},
			Healthcheck: &container.HealthConfig{Test: append([]string{"CMD"}, sqlCommand("client", "SELECT 1")...), Interval: 30 * time.Second, Timeout: 5 * time.Second, StartPeriod: 60 * time.Second, Retries: 3},
		},
		HostConfig: &container.HostConfig{
			NetworkMode:   container.NetworkMode(name),
			PortBindings:  network.PortMap{mysqlPort: []network.PortBinding{{HostIP: netip.MustParseAddr(c.cfg.BindIP), HostPort: strconv.Itoa(p.Port)}}},
			RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
			Resources:     container.Resources{NanoCPUs: int64(p.CPU) * 1e9, Memory: p.Memory * 1024 * 1024, MemorySwap: p.Memory * 1024 * 1024, PidsLimit: &pids},
			CapDrop:       []string{"NET_RAW"}, SecurityOpt: []string{"no-new-privileges:true"},
			LogConfig: container.LogConfig{Type: "local", Config: map[string]string{"max-size": "10m", "max-file": "3"}},
			Mounts: []mount.Mount{
				{Type: mount.TypeVolume, Source: name, Target: "/var/lib/mysql"},
				{Type: mount.TypeBind, Source: filepath.Join(c.dir(id), "secrets"), Target: "/run/fulcrum-secrets", ReadOnly: true},
				{Type: mount.TypeBind, Source: filepath.Join(c.dir(id), "config"), Target: "/etc/mysql/conf.d", ReadOnly: true},
			},
		},
	}
}

func (c *Client) updateResources(ctx context.Context, id string, cpu int, memory int64) error {
	result, err := c.api.ContainerUpdate(ctx, id, resourceOptions(cpu, memory))
	if err != nil {
		return err
	}
	if len(result.Warnings) > 0 {
		return fmt.Errorf("Docker resource warnings: %v", result.Warnings)
	}
	return nil
}

func resourceOptions(cpu int, memory int64) client.ContainerUpdateOptions {
	return client.ContainerUpdateOptions{Resources: &container.Resources{NanoCPUs: int64(cpu) * 1e9, Memory: memory * 1024 * 1024, MemorySwap: memory * 1024 * 1024}}
}

func (c *Client) checkLayout(v *container.InspectResponse, s *state) error {
	if v == nil {
		return fmt.Errorf("container missing")
	}
	if v.Config.Image != s.Image || string(v.HostConfig.NetworkMode) != resourceName(s.ServiceID) {
		return fmt.Errorf("container image or network differs from managed configuration")
	}
	binds := v.HostConfig.PortBindings[mysqlPort]
	if len(binds) != 1 || binds[0].HostIP.String() != s.BindIP || binds[0].HostPort != strconv.Itoa(s.Port) {
		return fmt.Errorf("container endpoint drift detected")
	}
	for _, m := range v.Mounts {
		if m.Destination == "/var/lib/mysql" && m.Name == resourceName(s.ServiceID) {
			return nil
		}
	}
	return fmt.Errorf("container data volume drift detected")
}

func (c *Client) start(ctx context.Context, id string, s *state, v *container.InspectResponse) (*mysqlagent.Instance, error) {
	if v.State.Paused || v.State.Restarting {
		return nil, fmt.Errorf("container paused or restarting; stop explicitly before retry")
	}
	// A failed port binding can detach the endpoint. Restore only our owned network.
	if v.NetworkSettings == nil || v.NetworkSettings.Networks[resourceName(id)] == nil {
		if _, err := c.api.NetworkConnect(ctx, resourceName(id), client.NetworkConnectOptions{Container: v.ID}); err != nil {
			return nil, err
		}
	}
	if !v.State.Running {
		if _, err := c.api.ContainerStart(ctx, v.ID, client.ContainerStartOptions{}); err != nil {
			return nil, err
		}
	}
	if err := c.waitReady(ctx, v.ID); err != nil {
		return nil, err
	}
	return c.instance(id, s, v.ID), nil
}

func (c *Client) Start(ctx context.Context, id string) (*mysqlagent.Instance, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := validateID(id); err != nil {
		return nil, err
	}
	if _, err := c.preflight(ctx, id); err != nil {
		return nil, err
	}
	s, err := c.loadState(id)
	if err != nil {
		return nil, err
	}
	v, err := c.inspect(ctx, id)
	if err != nil {
		return nil, err
	}
	if err = c.checkLayout(v, s); err != nil {
		return nil, err
	}
	return c.start(ctx, id, s, v)
}

func active(v *container.InspectResponse) bool {
	return v.State.Running || v.State.Restarting || v.State.Paused || v.State.Status == "removing"
}

func (c *Client) Stop(ctx context.Context, id string) (*mysqlagent.Instance, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := validateID(id); err != nil {
		return nil, err
	}
	v, err := c.inspect(ctx, id)
	if err != nil {
		return nil, err
	}
	s, stateErr := c.loadState(id)
	if stateErr != nil && !errors.Is(stateErr, os.ErrNotExist) {
		return nil, stateErr
	}
	if v == nil {
		return c.instance(id, s, ""), nil
	}
	if v.State.Paused {
		return nil, fmt.Errorf("container paused outside the agent; unpause before stopping")
	}
	if active(v) {
		// No forced kill: cancellation reports failure while Docker may finish shutdown.
		forever := -1
		if _, err = c.api.ContainerStop(ctx, v.ID, client.ContainerStopOptions{Timeout: &forever}); err != nil {
			return nil, err
		}
	}
	v, err = c.inspect(ctx, id)
	if err != nil {
		return nil, err
	}
	if v != nil && active(v) {
		return nil, fmt.Errorf("container has not stopped")
	}
	if v == nil {
		return c.instance(id, s, ""), nil
	}
	return c.instance(id, s, v.ID), nil
}

func (c *Client) Update(ctx context.Context, id string, cpu int, memory int64) (*mysqlagent.Instance, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := validateID(id); err != nil {
		return nil, err
	}
	if err := mysqlagent.ValidateResources(cpu, memory, c.cfg); err != nil {
		return nil, err
	}
	if _, err := c.preflight(ctx, id); err != nil {
		return nil, err
	}
	s, err := c.loadState(id)
	if err != nil {
		return nil, err
	}
	v, err := c.inspect(ctx, id)
	if err != nil {
		return nil, err
	}
	if err = c.checkLayout(v, s); err != nil {
		return nil, err
	}
	if active(v) {
		return nil, fmt.Errorf("container must be stopped before update")
	}
	if err = c.updateResources(ctx, v.ID, cpu, memory); err != nil {
		return nil, err
	}
	if err = writeMySQLConfig(filepath.Join(c.dir(id), "config"), memory); err != nil {
		return nil, err
	}
	return c.instance(id, s, v.ID), nil
}

func (c *Client) Delete(ctx context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := validateID(id); err != nil {
		return err
	}
	if _, err := c.preflight(ctx, id); err != nil {
		return err
	}
	if _, err := c.loadState(id); err != nil {
		if _, e := os.Lstat(c.dir(id)); !errors.Is(e, os.ErrNotExist) {
			return err
		}
	}
	v, err := c.inspect(ctx, id)
	if err != nil {
		return err
	}
	if v != nil {
		if active(v) {
			return fmt.Errorf("container must be stopped before delete")
		}
		if _, err = c.api.ContainerRemove(ctx, v.ID, client.ContainerRemoveOptions{}); err != nil {
			return err
		}
	}
	if _, err = c.api.VolumeRemove(ctx, resourceName(id), client.VolumeRemoveOptions{}); err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	if _, err = c.api.NetworkRemove(ctx, resourceName(id), client.NetworkRemoveOptions{}); err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	return os.RemoveAll(c.dir(id))
}

func sqlCommand(account, sql string) []string {
	// Closing Docker exec attachment does not guarantee terminating the process.
	// Bound the native probe as well as its API context to prevent orphaned clients.
	cmd := []string{"timeout", "--kill-after=1s", "4s", "mysql", "--defaults-extra-file=/run/fulcrum-secrets/" + account + ".cnf", "--connect-timeout=3"}
	if account == "admin" {
		cmd = append(cmd, "--protocol=socket")
	} else {
		cmd = append(cmd, "--protocol=tcp", "--host=127.0.0.1", "--port=3306", "--ssl-mode=DISABLED", "--get-server-public-key")
	}
	return append(cmd, "--batch", "--skip-column-names", "--execute", sql)
}

func (c *Client) sql(ctx context.Context, containerID, account, query string, out io.Writer) error {
	exec, err := c.api.ExecCreate(ctx, containerID, client.ExecCreateOptions{User: "root", AttachStdout: true, AttachStderr: true, Cmd: sqlCommand(account, query)})
	if err != nil {
		return err
	}
	attached, err := c.api.ExecAttach(ctx, exec.ID, client.ExecAttachOptions{})
	if err != nil {
		return err
	}
	defer attached.Close()
	// Docker exec's hijacked connection needs explicit cancellation after attachment.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			attached.Close()
		case <-done:
		}
	}()
	if err = copyOutput(out, attached.Reader); err != nil {
		return err
	}
	status, err := c.api.ExecInspect(ctx, exec.ID, client.ExecInspectOptions{})
	if err != nil {
		return err
	}
	if status.Running || status.ExitCode != 0 {
		return fmt.Errorf("SQL check failed (credentials and server output redacted)")
	}
	return nil
}

func (c *Client) endpointReady(ctx context.Context, containerID string) error {
	v, err := c.api.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return err
	}
	if v.Container.NetworkSettings == nil {
		return fmt.Errorf("container network unavailable")
	}
	bindings := v.Container.NetworkSettings.Ports[mysqlPort]
	if len(bindings) != 1 || bindings[0].HostIP.String() != c.cfg.BindIP {
		return fmt.Errorf("published endpoint unavailable")
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(c.cfg.BindIP, bindings[0].HostPort))
	if err != nil {
		return err
	}
	return conn.Close()
}

func (c *Client) waitReady(ctx context.Context, containerID string) error {
	for {
		attempt, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := c.sql(attempt, containerID, "client", "SELECT 1", io.Discard)
		if err == nil {
			err = c.endpointReady(attempt, containerID)
		}
		cancel()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("MySQL did not become ready; resources retained: %w", ctx.Err())
		case <-time.After(time.Second):
		}
	}
}
