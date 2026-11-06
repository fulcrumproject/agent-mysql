// Configuration tests reject remote runtimes and accidentally public database bindings.
// They also check provider resource and timeout bounds before any provisioning.
package config

import (
	"github.com/fulcrumproject/utils/confbuilder"
	"testing"
	"time"
)

func TestExampleBootstrapConfiguration(t *testing.T) {
	path := "../../examples/config.json"
	cfg, err := confbuilder.New(DefaultConfig()).EnvPrefix("MYSQL_AGENT_TEST_").File(&path).Build()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.JobPollInterval != 5*time.Second || cfg.MetricReportInterval != 30*time.Second {
		t.Fatal("example durations not parsed")
	}
}

func TestCanonicalBindAddress(t *testing.T) {
	for input, want := range map[string]string{"fd00:0000::1": "fd00::1", "::ffff:127.0.0.1": "127.0.0.1"} {
		got := (DockerConfig{BindIP: input}).WithDefaults().BindIP
		if got != want {
			t.Fatalf("%s normalized to %s, want %s", input, got, want)
		}
	}
}

func TestDockerConfig(t *testing.T) {
	base := DockerConfig{BindIP: "10.0.0.1", Host: "mysql.internal"}.WithDefaults()
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, ip := range []string{"0.0.0.0", "::", "8.8.8.8", "not-an-ip"} {
		c := base
		c.BindIP = ip
		if c.Validate() == nil {
			t.Errorf("accepted public/invalid IP %s", ip)
		}
	}
	for _, ip := range []string{"127.0.0.1", "::1", "fd00::1", "192.168.1.4"} {
		c := base
		c.BindIP = ip
		if err := c.Validate(); err != nil {
			t.Errorf("%s: %v", ip, err)
		}
	}
	cases := []DockerConfig{base, base, base, base, base}
	cases[0].SocketPath = "tcp://host:2375"
	cases[1].StateDir = "/"
	cases[2].MaxMemory = 512
	cases[3].Host = "8.8.8.8"
	cases[4].StopTimeout = 9999
	for _, c := range cases {
		if c.Validate() == nil {
			t.Errorf("accepted invalid config: %+v", c)
		}
	}
}
