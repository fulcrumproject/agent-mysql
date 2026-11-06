// Runtime statistics are normalized to container limits and measured via Docker.
// SQL errors are reduced to availability=0 without exposing server or credential output.
package dockerapi

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"runtime"
	"time"

	"github.com/fulcrumproject/agent-mysql/internal/mysqlagent"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"golang.org/x/sys/unix"
)

func copyOutput(out io.Writer, in io.Reader) error {
	_, err := stdcopy.StdCopy(out, io.Discard, in)
	return err
}

func (c *Client) Metrics(ctx context.Context, id string) (mysqlagent.Metrics, error) {
	var m mysqlagent.Metrics
	if err := validateID(id); err != nil {
		return m, err
	}
	v, err := c.inspect(ctx, id)
	if err != nil {
		return m, err
	}
	// Local volumes use the daemon's data-root filesystem. This is shared capacity.
	info, err := c.api.Info(ctx, client.InfoOptions{})
	if err != nil {
		return m, err
	}
	var fs unix.Statfs_t
	if err = unix.Statfs(info.Info.DockerRootDir, &fs); err != nil {
		return m, err
	}
	m.StorageFreeBytes = float64(fs.Bavail) * float64(fs.Bsize)
	if v == nil || !v.State.Running || v.State.Restarting || v.State.Paused {
		return m, nil
	}
	stats, err := c.api.ContainerStats(ctx, v.ID, client.ContainerStatsOptions{IncludePreviousSample: true})
	if err != nil {
		return m, err
	}
	defer stats.Body.Close()
	var s container.StatsResponse
	if err = json.NewDecoder(stats.Body).Decode(&s); err != nil {
		return m, err
	}
	m.CPU, m.Memory = usage(s, v.HostConfig.NanoCPUs, v.HostConfig.Memory)
	if started, err := time.Parse(time.RFC3339Nano, v.State.StartedAt); err == nil {
		m.Uptime = math.Max(0, time.Since(started).Seconds())
	}
	probe, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if c.sql(probe, v.ID, "client", "SELECT 1", io.Discard) == nil {
		m.Available = 1
	}
	return m, nil
}

func usage(s container.StatsResponse, nanoCPUs, memory int64) (cpu, mem float64) {
	if nanoCPUs > 0 && s.CPUStats.CPUUsage.TotalUsage >= s.PreCPUStats.CPUUsage.TotalUsage && s.CPUStats.SystemUsage > s.PreCPUStats.SystemUsage {
		n := s.CPUStats.OnlineCPUs
		if n == 0 {
			n = uint32(runtime.NumCPU())
		}
		cpu = float64(s.CPUStats.CPUUsage.TotalUsage-s.PreCPUStats.CPUUsage.TotalUsage) / float64(s.CPUStats.SystemUsage-s.PreCPUStats.SystemUsage) * float64(n) / (float64(nanoCPUs) / 1e9)
	}
	if memory > 0 {
		mem = float64(s.MemoryStats.Usage) / float64(memory)
	}
	return cpu, mem
}
