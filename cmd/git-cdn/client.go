package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/spf13/cobra"
)

// adminClient talks to a node's loopback operations endpoint. The
// commands are for an operator on the node itself (or behind an SSH
// tunnel); the endpoint carries no authentication by design.
type adminClient struct {
	addr string
	http *http.Client
}

func newAdminClient(addr string, timeout time.Duration) *adminClient {
	return &adminClient{addr: addr, http: &http.Client{Timeout: timeout}}
}

func addAdminFlags(cmd *cobra.Command, addr *string, timeout *time.Duration, asJSON *bool) {
	cmd.Flags().StringVar(addr, "addr", "http://127.0.0.1:8081", "Admin endpoint of the node")
	cmd.Flags().DurationVar(timeout, "timeout", 10*time.Second, "Request timeout")
	cmd.Flags().BoolVar(asJSON, "json", false, "Print the raw JSON response")
}

// call performs one request and returns the raw body, decoding it into
// out when given. On a non-200 it unwraps the endpoint's own error
// message, so validation failures read as their cause instead of a bare
// status.
func (c *adminClient) call(method, path string, out any) ([]byte, error) {
	r, err := http.NewRequest(method, c.addr+path, nil)
	if err != nil {
		return nil, err
	}
	response, err := c.http.Do(r)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		var payload struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &payload) == nil && payload.Error != "" {
			return nil, fmt.Errorf("%s: %s", response.Status, payload.Error)
		}
		return nil, fmt.Errorf("%s", response.Status)
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return nil, err
		}
	}
	return body, nil
}

// formatBytes renders sizes in binary units, matching the site's metric
// vocabulary.
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	for _, name := range units {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, name)
		}
	}
	return fmt.Sprintf("%.1f PiB", value)
}

// formatUptime renders seconds the way operators read them.
func formatUptime(seconds float64) string {
	if seconds < 90 {
		return fmt.Sprintf("%.0fs", seconds)
	}
	minutes := seconds / 60
	if minutes < 90 {
		return fmt.Sprintf("%.0fm", minutes)
	}
	hours := minutes / 60
	if hours < 36 {
		return fmt.Sprintf("%.1fh", hours)
	}
	return fmt.Sprintf("%.1fd", hours/24)
}
