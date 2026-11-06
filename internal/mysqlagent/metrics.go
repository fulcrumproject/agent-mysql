package mysqlagent

import (
	"context"
	"time"

	"github.com/fulcrumproject/agent-lib-go/pkg/agent"
)

func (a *MySQLAgent) MetricsReporter() agent.MetricsReporter[Properties, Instance] {
	return func(ctx context.Context, s *agent.Service[Properties, Instance]) ([]agent.MetricEntry, error) {
		if err := a.validateService(s); err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		m, err := a.docker.Metrics(ctx, s.ID)
		if err != nil {
			return nil, err
		}
		values := []struct {
			name  string
			value float64
		}{
			{"mysql.cpu.usage", m.CPU}, {"mysql.memory.usage", m.Memory}, {"mysql.uptime", m.Uptime},
			{"mysql.available", m.Available}, {"mysql.host.disk.free", m.StorageFreeBytes},
		}
		entries := make([]agent.MetricEntry, 0, len(values))
		for _, v := range values {
			entries = append(entries, agent.MetricEntry{TypeName: v.name, Value: v.value, ResourceID: s.ID, AgentInstanceID: s.ID})
		}
		return entries, nil
	}
}
