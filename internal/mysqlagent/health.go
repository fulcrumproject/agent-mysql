package mysqlagent

import (
	"context"
	"fmt"
	"time"

	"github.com/fulcrumproject/agent-lib-go/pkg/agent"
)

// A failing database must not block jobs for other services.
func (a *MySQLAgent) HealthHandler() agent.HealthHandler {
	return func(ctx context.Context) error {
		if a.docker == nil {
			return fmt.Errorf("Docker client not initialized")
		}
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return a.docker.Ping(ctx)
	}
}
