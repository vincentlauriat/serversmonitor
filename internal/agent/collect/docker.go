package collect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type dockerReader struct {
	socket string
	client *http.Client
}

// NewDocker talks to the Engine API over a unix socket with plain HTTP.
// Only two read-only endpoints are used: /containers/json and /containers/{id}/stats.
func NewDocker(socketPath string) DockerReader {
	return &dockerReader{socket: socketPath, client: &http.Client{
		Timeout: 8 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		}},
	}}
}

type dockerContainer struct {
	ID     string   `json:"Id"`
	Names  []string `json:"Names"`
	Image  string   `json:"Image"`
	State  string   `json:"State"`
	Status string   `json:"Status"`
}

type dockerStats struct {
	CPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemCPUUsage uint64 `json:"system_cpu_usage"`
		OnlineCPUs     uint64 `json:"online_cpus"`
	} `json:"cpu_stats"`
	PreCPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemCPUUsage uint64 `json:"system_cpu_usage"`
	} `json:"precpu_stats"`
	MemoryStats struct {
		Usage uint64            `json:"usage"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
	Networks map[string]struct {
		RxBytes uint64 `json:"rx_bytes"`
		TxBytes uint64 `json:"tx_bytes"`
	} `json:"networks"`
}

func (d *dockerReader) get(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", "http://docker"+path, nil)
	if err != nil {
		return err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("docker %s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func (d *dockerReader) Containers(ctx context.Context) ([]ContainerStat, error) {
	if _, err := os.Stat(d.socket); err != nil {
		return nil, ErrUnavailable
	}
	var list []dockerContainer
	if err := d.get(ctx, "/containers/json", &list); err != nil {
		var ne net.Error
		if errors.As(err, &ne) || errors.Is(err, os.ErrNotExist) {
			return nil, ErrUnavailable
		}
		return nil, err
	}
	out := make([]ContainerStat, len(list))
	var wg sync.WaitGroup
	for i, c := range list {
		wg.Add(1)
		go func(i int, c dockerContainer) {
			defer wg.Done()
			name := c.ID
			if len(name) > 12 {
				name = name[:12]
			}
			if len(c.Names) > 0 {
				name = strings.TrimPrefix(c.Names[0], "/")
			}
			st := ContainerStat{Name: name, Image: c.Image, Status: c.State}
			var stats dockerStats
			if err := d.get(ctx, "/containers/"+c.ID+"/stats?stream=false&one-shot=true", &stats); err == nil {
				cpuDelta := float64(stats.CPUStats.CPUUsage.TotalUsage) - float64(stats.PreCPUStats.CPUUsage.TotalUsage)
				sysDelta := float64(stats.CPUStats.SystemCPUUsage) - float64(stats.PreCPUStats.SystemCPUUsage)
				cpus := float64(stats.CPUStats.OnlineCPUs)
				if cpus == 0 {
					cpus = 1
				}
				if sysDelta > 0 && cpuDelta >= 0 {
					st.CPU = cpuDelta / sysDelta * cpus * 100
				}
				// Docker's "usage" counts page cache; the inactive file pages are
				// what `docker stats` subtracts to show real memory.
				usage := stats.MemoryStats.Usage
				if inactive := stats.MemoryStats.Stats["inactive_file"]; inactive < usage {
					usage -= inactive
				}
				st.MemUsed = int64(usage)
				for _, n := range stats.Networks {
					st.NetSent += int64(n.TxBytes)
					st.NetRecv += int64(n.RxBytes)
				}
			}
			out[i] = st
		}(i, c)
	}
	wg.Wait()
	return out, nil
}
