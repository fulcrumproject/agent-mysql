package mysqlagent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/fulcrumproject/agent-lib-go/pkg/agent"
)

func (a *MySQLAgent) CreateHandler() agent.JobHandler[Properties, Properties, Instance] {
	return func(ctx context.Context, j *agent.Job[Properties, Properties, Instance]) (*agent.JobResponse[Instance], error) {
		if j == nil {
			return nil, fmt.Errorf("missing job")
		}
		if err := a.validateService(j.Service); err != nil {
			return nil, err
		}
		if j.Service.Status != agent.ServiceStatusNew {
			return nil, fmt.Errorf("create requires New state")
		}
		if j.Service.Properties == nil {
			return nil, fmt.Errorf("missing service properties")
		}
		p := *j.Service.Properties
		// Core sends properties on the first create; action retries have no parameters.
		if j.Params != nil && j.Params.Database != "" {
			p = *j.Params
		}
		if strings.HasPrefix(p.Password, "vault://") {
			value, err := a.core.GetSecret(strings.TrimPrefix(p.Password, "vault://"))
			if err != nil {
				return nil, fmt.Errorf("cannot resolve database password")
			}
			var ok bool
			p.Password, ok = value.(string)
			if !ok {
				return nil, fmt.Errorf("database password is not a string")
			}
		}
		if err := p.Validate(a.cfg); err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(ctx, time.Duration(a.cfg.OperationTimeout)*time.Second)
		defer cancel()
		instance, err := a.docker.Create(ctx, j.Service.ID, p)
		if err != nil {
			return nil, err
		}
		return response(j.Service.ID, instance), nil
	}
}

func (a *MySQLAgent) StartHandler() agent.JobHandler[struct{}, Properties, Instance] {
	return func(ctx context.Context, j *agent.Job[struct{}, Properties, Instance]) (*agent.JobResponse[Instance], error) {
		if j == nil {
			return nil, fmt.Errorf("missing job")
		}
		if err := a.validateService(j.Service); err != nil {
			return nil, err
		}
		if j.Service.Status != agent.ServiceStatusStopped {
			return nil, fmt.Errorf("start requires Stopped state")
		}
		ctx, cancel := context.WithTimeout(ctx, time.Duration(a.cfg.OperationTimeout)*time.Second)
		defer cancel()
		v, err := a.docker.Start(ctx, j.Service.ID)
		if err != nil {
			return nil, err
		}
		return response(j.Service.ID, v), nil
	}
}

func (a *MySQLAgent) StopHandler() agent.JobHandler[struct{}, Properties, Instance] {
	return func(ctx context.Context, j *agent.Job[struct{}, Properties, Instance]) (*agent.JobResponse[Instance], error) {
		if j == nil {
			return nil, fmt.Errorf("missing job")
		}
		if err := a.validateService(j.Service); err != nil {
			return nil, err
		}
		// New permits recovery of a failed or timed-out create before explicit deletion.
		switch j.Service.Status {
		case agent.ServiceStatusNew, agent.ServiceStatusStarted, agent.ServiceStatusStopped:
		default:
			return nil, fmt.Errorf("stop not allowed in this state")
		}
		ctx, cancel := context.WithTimeout(ctx, time.Duration(a.cfg.StopTimeout)*time.Second)
		defer cancel()
		v, err := a.docker.Stop(ctx, j.Service.ID)
		if err != nil {
			return nil, err
		}
		return response(j.Service.ID, v), nil
	}
}

func (a *MySQLAgent) UpdateHandler() agent.JobHandler[UpdateProperties, Properties, Instance] {
	return func(ctx context.Context, j *agent.Job[UpdateProperties, Properties, Instance]) (*agent.JobResponse[Instance], error) {
		if j == nil {
			return nil, fmt.Errorf("missing job")
		}
		if err := a.validateService(j.Service); err != nil {
			return nil, err
		}
		if j.Service.Status != agent.ServiceStatusStopped {
			return nil, fmt.Errorf("update requires Stopped state")
		}
		if j.Service.Properties == nil || j.Params == nil {
			return nil, fmt.Errorf("missing resource properties")
		}
		old := j.Service.Properties
		if (j.Params.Database != nil && *j.Params.Database != old.Database) ||
			(j.Params.Username != nil && *j.Params.Username != old.Username) ||
			(j.Params.Port != nil && *j.Params.Port != old.Port) {
			return nil, fmt.Errorf("database, username and port are immutable")
		}
		if j.Params.Password != nil {
			password := old.Password
			if strings.HasPrefix(password, "vault://") {
				value, err := a.core.GetSecret(strings.TrimPrefix(password, "vault://"))
				if err != nil {
					return nil, fmt.Errorf("cannot verify immutable database password")
				}
				var ok bool
				password, ok = value.(string)
				if !ok {
					return nil, fmt.Errorf("invalid stored password type")
				}
			}
			if *j.Params.Password != password {
				return nil, fmt.Errorf("password rotation is not supported")
			}
		}
		cpu, memory := old.CPU, old.Memory
		if j.Params.CPU != nil {
			cpu = *j.Params.CPU
		}
		if j.Params.Memory != nil {
			memory = *j.Params.Memory
		}
		if err := ValidateResources(cpu, memory, a.cfg); err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(ctx, time.Duration(a.cfg.OperationTimeout)*time.Second)
		defer cancel()
		v, err := a.docker.Update(ctx, j.Service.ID, cpu, memory)
		if err != nil {
			return nil, err
		}
		return response(j.Service.ID, v), nil
	}
}

func (a *MySQLAgent) DeleteHandler() agent.JobHandler[struct{}, Properties, Instance] {
	return func(ctx context.Context, j *agent.Job[struct{}, Properties, Instance]) (*agent.JobResponse[Instance], error) {
		if j == nil {
			return nil, fmt.Errorf("missing job")
		}
		if err := a.validateService(j.Service); err != nil {
			return nil, err
		}
		if j.Service.Status != agent.ServiceStatusStopped {
			return nil, fmt.Errorf("delete requires Stopped state")
		}
		ctx, cancel := context.WithTimeout(ctx, time.Duration(a.cfg.OperationTimeout)*time.Second)
		defer cancel()
		if err := a.docker.Delete(ctx, j.Service.ID); err != nil {
			return nil, err
		}
		return &agent.JobResponse[Instance]{}, nil
	}
}
