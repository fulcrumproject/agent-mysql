package dockerapi

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/fulcrumproject/agent-mysql/internal/config"
	"github.com/fulcrumproject/agent-mysql/internal/mysqlagent"
)

type state struct {
	AgentID   string `json:"agentId"`
	ServiceID string `json:"serviceId"`
	Host      string `json:"host"`
	BindIP    string `json:"bindIp"`
	Port      int    `json:"port"`
	Database  string `json:"database"`
	Username  string `json:"username"`
	Image     string `json:"image"`
}

func (c *Client) dir(id string) string { return filepath.Join(c.cfg.StateDir, c.agentID, id) }

func privateDir(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("state directory must be a real directory with mode 0700: %s", path)
	}
	return nil
}

func (c *Client) loadState(id string) (*state, error) {
	dir := c.dir(id)
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("unsafe service state directory")
	}
	b, err := os.ReadFile(filepath.Join(dir, "owner.json"))
	if err != nil {
		return nil, fmt.Errorf("incomplete service state: %w", err)
	}
	var s state
	if err = json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("invalid service state")
	}
	if s.AgentID != c.agentID || s.ServiceID != id {
		return nil, fmt.Errorf("service state ownership mismatch")
	}
	return &s, nil
}

func (c *Client) ensureState(id string, p mysqlagent.Properties, hasResources bool) (*state, error) {
	s, err := c.loadState(id)
	if err == nil {
		if s.Database != p.Database || s.Username != p.Username || s.Port != p.Port || s.Host != c.cfg.Host || s.BindIP != c.cfg.BindIP || s.Image != config.Image {
			return nil, fmt.Errorf("immutable service configuration differs from existing state")
		}
		for _, file := range []string{"root-password", "bootstrap-password", "client.cnf", "admin.cnf"} {
			info, err := os.Lstat(filepath.Join(c.dir(id), "secrets", file))
			if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
				return nil, fmt.Errorf("stored credentials unavailable or unsafe")
			}
		}
		password, err := os.ReadFile(filepath.Join(c.dir(id), "secrets", "password"))
		if err != nil {
			return nil, fmt.Errorf("stored credentials unavailable")
		}
		if subtle.ConstantTimeCompare(password, []byte(p.Password)) != 1 {
			return nil, fmt.Errorf("password rotation is not supported")
		}
		return s, nil
	}
	// Only a missing directory is a new service; never regenerate partial or lost credentials.
	if _, statErr := os.Lstat(c.dir(id)); !errors.Is(statErr, os.ErrNotExist) || hasResources {
		return nil, fmt.Errorf("existing resources require intact service state: %w", err)
	}
	root := make([]byte, 32)
	if _, err = rand.Read(root); err != nil {
		return nil, err
	}
	rootPassword := hex.EncodeToString(root)
	s = &state{AgentID: c.agentID, ServiceID: id, Host: c.cfg.Host, BindIP: c.cfg.BindIP, Port: p.Port, Database: p.Database, Username: p.Username, Image: config.Image}
	parent := filepath.Dir(c.dir(id))
	tmp, err := os.MkdirTemp(parent, ".prepare-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	if err = os.Mkdir(filepath.Join(tmp, "secrets"), 0700); err != nil {
		return nil, err
	}
	if err = os.Mkdir(filepath.Join(tmp, "config"), 0755); err != nil {
		return nil, err
	}
	// The daemon drops to mysql after bootstrap, even if the agent uses umask 0077.
	if err = os.Chmod(filepath.Join(tmp, "config"), 0755); err != nil {
		return nil, err
	}
	data, _ := json.Marshal(s)
	files := map[string]string{
		"owner.json":            string(data),
		"secrets/root-password": rootPassword,
		"secrets/password":      p.Password,
		// The pinned image embeds MYSQL_PASSWORD in a SQL literal during bootstrap.
		"secrets/bootstrap-password": strings.ReplaceAll(strings.ReplaceAll(p.Password, "\\", "\\\\"), "'", "''"),
		"secrets/client.cnf":         clientConfig(p.Username, p.Password, p.Database),
		"secrets/admin.cnf":          clientConfig("root", rootPassword, "mysql"),
	}
	for name, value := range files {
		if err = writePrivateFile(filepath.Join(tmp, name), []byte(value)); err != nil {
			return nil, err
		}
	}
	if err = writeMySQLConfig(filepath.Join(tmp, "config"), p.Memory); err != nil {
		return nil, err
	}
	for _, dir := range []string{filepath.Join(tmp, "secrets"), filepath.Join(tmp, "config"), tmp} {
		if err = syncDir(dir); err != nil {
			return nil, err
		}
	}
	if err = os.Rename(tmp, c.dir(id)); err != nil {
		return nil, err
	}
	if err = syncDir(parent); err != nil {
		return nil, err
	}
	return s, nil
}

func writePrivateFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func clientConfig(user, password, database string) string {
	// MySQL option-file syntax, not SQL or shell syntax.
	escaped := strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "\t", "\\t").Replace(password)
	return fmt.Sprintf("[client]\nuser=%s\npassword=\"%s\"\ndatabase=%s\n", user, escaped, database)
}

func writeMySQLConfig(dir string, memory int64) error {
	// Leave headroom for the server, buffers and connections instead of sizing to host RAM.
	content := fmt.Sprintf("[mysqld]\ninnodb_buffer_pool_size=%d\nmax_connections=50\nmysqlx=0\n", memory*1024*1024/2)
	f, err := os.CreateTemp(dir, ".config-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.WriteString(content); err != nil {
		f.Close()
		return err
	}
	if err = f.Chmod(0644); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, filepath.Join(dir, "fulcrum.cnf")); err != nil {
		return err
	}
	return syncDir(dir)
}

func (c *Client) instance(id string, s *state, containerID string) *mysqlagent.Instance {
	if s == nil {
		return nil
	}
	return &mysqlagent.Instance{ContainerID: containerID, Volume: resourceName(id), Host: s.Host, Port: s.Port, Database: s.Database, Username: s.Username}
}
