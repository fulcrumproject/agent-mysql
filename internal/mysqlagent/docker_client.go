package mysqlagent

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/fulcrumproject/agent-mysql/internal/config"
)

type Properties struct {
	CPU      int    `json:"cpu"`
	Memory   int64  `json:"memory"` // MiB
	Database string `json:"database"`
	Username string `json:"username"`
	Password string `json:"password"`
	Port     int    `json:"port"`
}

type UpdateProperties struct {
	CPU    *int   `json:"cpu,omitempty"`
	Memory *int64 `json:"memory,omitempty"`
	// Core may send merged properties. These fields are checked, never changed.
	Database *string `json:"database,omitempty"`
	Username *string `json:"username,omitempty"`
	Password *string `json:"password,omitempty"`
	Port     *int    `json:"port,omitempty"`
}

// Instance is returned to Core; never include secrets.
type Instance struct {
	ContainerID string `json:"containerId"`
	Volume      string `json:"volume"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Database    string `json:"database"`
	Username    string `json:"username"`
}

type Metrics struct {
	CPU              float64
	Memory           float64
	Uptime           float64
	Available        float64
	StorageFreeBytes float64
}

type DockerClient interface {
	Ping(context.Context) error
	Create(context.Context, string, Properties) (*Instance, error)
	Start(context.Context, string) (*Instance, error)
	Stop(context.Context, string) (*Instance, error)
	Update(context.Context, string, int, int64) (*Instance, error)
	Delete(context.Context, string) error
	Metrics(context.Context, string) (Metrics, error)
	Close() error
}

type ClientFactory func(config.DockerConfig, string) (DockerClient, error)

var identifier = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*$`)
var serviceID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func ValidID(id string) bool { return serviceID.MatchString(id) }

func ValidateResources(cpu int, memory int64, c config.DockerConfig) error {
	if cpu < 1 || cpu > c.MaxCPU || memory < 1024 || memory > c.MaxMemory {
		return fmt.Errorf("CPU or memory outside provider limits (memory is in MiB)")
	}
	return nil
}

func (p Properties) Validate(c config.DockerConfig) error {
	if err := ValidateResources(p.CPU, p.Memory, c); err != nil {
		return err
	}
	if !identifier.MatchString(p.Database) || len(p.Database) > 64 {
		return fmt.Errorf("invalid database name")
	}
	switch strings.ToLower(p.Database) {
	case "mysql", "sys", "performance_schema", "information_schema":
		return fmt.Errorf("system database names are reserved")
	}
	if !identifier.MatchString(p.Username) || len(p.Username) > 32 || strings.EqualFold(p.Username, "root") || strings.HasPrefix(strings.ToLower(p.Username), "mysql") {
		return fmt.Errorf("invalid or reserved username")
	}
	if len(p.Password) < 16 || len(p.Password) > 256 || strings.ContainsAny(p.Password, "\x00\r\n") || strings.HasPrefix(p.Password, "vault://") {
		return fmt.Errorf("password must be a resolved secret of 16..256 bytes without line breaks")
	}
	if p.Port < 1024 || p.Port > 65535 {
		return fmt.Errorf("port must be allocated from 1024..65535")
	}
	return nil
}
