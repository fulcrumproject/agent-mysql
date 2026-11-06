package mysqlagent

import (
	"context"
	"errors"
	"fmt"

	"github.com/fulcrumproject/agent-lib-go/pkg/agent"
	"github.com/fulcrumproject/agent-lib-go/pkg/stdagent"
	"github.com/fulcrumproject/agent-mysql/internal/config"
)

type RemoteConfig struct {
	Docker config.DockerConfig `json:"docker"`
}

type MySQLAgent struct {
	std     *stdagent.Agent
	core    stdagent.FulcrumClient
	factory ClientFactory
	docker  DockerClient
	cfg     config.DockerConfig
	id      string
}

func NewMySQLAgent(factory ClientFactory, core stdagent.FulcrumClient, opts ...stdagent.AgentOption) (*MySQLAgent, error) {
	std, err := stdagent.New(core, opts...)
	if err != nil {
		return nil, err
	}
	a := &MySQLAgent{std: std, core: core, factory: factory}
	registrations := []error{
		std.OnConnect(agent.ConnectHandlerWrapper(a.ConnectHandler)),
		std.OnHealth(a.HealthHandler()),
		std.OnJob("create", agent.JobHandlerWrapper(a.CreateHandler())),
		std.OnJob("start", agent.JobHandlerWrapper(a.StartHandler())),
		std.OnJob("stop", agent.JobHandlerWrapper(a.StopHandler())),
		std.OnJob("update", agent.JobHandlerWrapper(a.UpdateHandler())),
		std.OnJob("delete", agent.JobHandlerWrapper(a.DeleteHandler())),
		std.OnMetrics(agent.MetricsReporterWrapper(a.MetricsReporter())),
	}
	return a, errors.Join(registrations...)
}

func (a *MySQLAgent) ConnectHandler(ctx context.Context, info *agent.AgentInfo[RemoteConfig]) error {
	if info == nil || info.Config == nil || !ValidID(info.ID) {
		return fmt.Errorf("missing agent identity/configuration")
	}
	cfg := info.Config.Docker.WithDefaults()
	if err := cfg.Validate(); err != nil {
		return err
	}
	backend, err := a.factory(cfg, info.ID)
	if err != nil {
		return err
	}
	if err = backend.Ping(ctx); err != nil {
		backend.Close()
		return err
	}
	a.docker = backend
	a.cfg = cfg
	a.id = info.ID
	return nil
}

func (a *MySQLAgent) Run(ctx context.Context) error { return a.std.Run(ctx) }
func (a *MySQLAgent) Shutdown(ctx context.Context) error {
	err := a.std.Shutdown(ctx)
	if a.docker != nil {
		err = errors.Join(err, a.docker.Close())
	}
	return err
}
func (a *MySQLAgent) GetAgentID() string { return a.std.GetAgentID() }

func (a *MySQLAgent) validateService(s *agent.Service[Properties, Instance]) error {
	if a.docker == nil {
		return fmt.Errorf("Docker client not initialized")
	}
	if s == nil || !ValidID(s.ID) || s.AgentID != a.id {
		return fmt.Errorf("invalid service or agent ownership")
	}
	return nil
}

func response(id string, instance *Instance) *agent.JobResponse[Instance] {
	return &agent.JobResponse[Instance]{AgentInstanceID: &id, AgentInstanceData: instance}
}
