package config

import (
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"regexp"
	"time"

	"github.com/fulcrumproject/utils/logging"
)

const EnvPrefix = "MYSQL_AGENT_"
const Image = "mysql:9.7.4@sha256:df90258da46f6e7ff8574ff84f17618aba0765cac749928299c69fd4e7cb8ad8"

// Config contains only bootstrap configuration, not database credentials.
type Config struct {
	AgentToken           string        `json:"agentToken" validate:"required" env:"FULCRUM_API_TOKEN"`
	FulcrumAPIURL        string        `json:"fulcrumApiUrl" validate:"required,url" env:"FULCRUM_API_URL"`
	JobPollInterval      time.Duration `json:"jobPollInterval" env:"JOB_POLL_INTERVAL"`
	MetricReportInterval time.Duration `json:"metricReportInterval" env:"METRIC_REPORT_INTERVAL"`
	LogConfig            logging.Conf  `json:"log" validate:"required"`
}

func DefaultConfig() *Config {
	return &Config{FulcrumAPIURL: "http://localhost:3000", JobPollInterval: 5 * time.Second, MetricReportInterval: 30 * time.Second}
}

// DockerConfig is provider-controlled; the customer cannot choose mounts or images.
type DockerConfig struct {
	SocketPath       string `json:"socketPath"`
	StateDir         string `json:"stateDir"`
	BindIP           string `json:"bindIp"`
	Host             string `json:"host"`
	MaxCPU           int    `json:"maxCpu"`
	MaxMemory        int64  `json:"maxMemory"`
	OperationTimeout int    `json:"operationTimeout"`
	StopTimeout      int    `json:"stopTimeout"`
}

func (c DockerConfig) WithDefaults() DockerConfig {
	// Docker normalizes addresses when reporting bindings; compare canonical values.
	if ip, err := netip.ParseAddr(c.BindIP); err == nil {
		c.BindIP = ip.Unmap().String()
	}
	if c.SocketPath == "" {
		c.SocketPath = "/var/run/docker.sock"
	}
	if c.StateDir == "" {
		c.StateDir = "/var/lib/fulcrum-agent-mysql"
	}
	if c.MaxCPU == 0 {
		c.MaxCPU = 64
	}
	if c.MaxMemory == 0 {
		c.MaxMemory = 262144
	}
	if c.OperationTimeout == 0 {
		c.OperationTimeout = 180
	}
	if c.StopTimeout == 0 {
		c.StopTimeout = 60
	}
	return c
}

var hostname = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.-]{0,252}$`)

func (c DockerConfig) Validate() error {
	if !filepath.IsAbs(c.SocketPath) || !filepath.IsAbs(c.StateDir) || filepath.Clean(c.StateDir) == "/" {
		return fmt.Errorf("socketPath and stateDir must be absolute local paths")
	}
	ip := net.ParseIP(c.BindIP)
	if ip == nil || (!ip.IsPrivate() && !ip.IsLoopback()) {
		return fmt.Errorf("bindIp must be a private or loopback IP, never a wildcard or public address")
	}
	if hostIP := net.ParseIP(c.Host); hostIP != nil {
		if !hostIP.IsPrivate() && !hostIP.IsLoopback() {
			return fmt.Errorf("host must advertise a private endpoint")
		}
	} else if !hostname.MatchString(c.Host) {
		return fmt.Errorf("host must be a private DNS hostname or IP")
	}
	if c.MaxCPU < 1 || c.MaxCPU > 64 || c.MaxMemory < 1024 || c.MaxMemory > 262144 {
		return fmt.Errorf("provider limits must be 1..64 CPUs and 1024..262144 MiB")
	}
	if c.OperationTimeout < 10 || c.OperationTimeout > 3600 || c.StopTimeout < 1 || c.StopTimeout > c.OperationTimeout {
		return fmt.Errorf("invalid operation/stop timeout")
	}
	return nil
}
