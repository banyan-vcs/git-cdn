package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

type statusPayload struct {
	Status            string  `json:"status"`
	Version           string  `json:"version"`
	Mode              string  `json:"mode"`
	Listen            string  `json:"listen"`
	UptimeSeconds     float64 `json:"uptime_seconds"`
	ActiveRequests    int     `json:"active_requests"`
	ActiveSSHSessions int     `json:"active_ssh_sessions"`
	Cache             struct {
		Entries       int   `json:"entries"`
		Bytes         int64 `json:"bytes"`
		MaxBytes      int64 `json:"max_bytes"`
		ReservedBytes int64 `json:"reserved_bytes"`
	} `json:"cache"`
	DiskFreeBytes *int64 `json:"disk_free_bytes"`
	Totals        struct {
		LocalRequests        uint64 `json:"local_requests"`
		PassthroughRequests  uint64 `json:"passthrough_requests"`
		HydratedRepositories uint64 `json:"hydrated_repositories"`
		RejectedRequests     uint64 `json:"rejected_requests"`
		PackCacheHits        uint64 `json:"pack_cache_hits"`
	} `json:"totals"`
}

func newStatusCommand() *cobra.Command {
	var (
		addr    string
		timeout time.Duration
		asJSON  bool
	)
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show node health, cache and traffic summary",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newAdminClient(addr, timeout)
			raw, err := client.call("GET", "/status", nil)
			if err != nil {
				return err
			}
			if asJSON {
				_, err = cmd.OutOrStdout().Write(raw)
				return err
			}
			var payload statusPayload
			if err := json.Unmarshal(raw, &payload); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			disk := "unknown"
			if payload.DiskFreeBytes != nil {
				disk = formatBytes(*payload.DiskFreeBytes) + " free"
			}
			share := 0.0
			if payload.Cache.MaxBytes > 0 {
				share = float64(payload.Cache.Bytes) / float64(payload.Cache.MaxBytes) * 100
			}
			for _, line := range []string{
				fmt.Sprintf("status    %s (version %s, uptime %s)", payload.Status, payload.Version, formatUptime(payload.UptimeSeconds)),
				fmt.Sprintf("mode      %s, listening on %s", payload.Mode, payload.Listen),
				fmt.Sprintf("cache     %d repositories, %s of %s (%.1f%%)", payload.Cache.Entries, formatBytes(payload.Cache.Bytes), formatBytes(payload.Cache.MaxBytes), share),
				fmt.Sprintf("disk      %s", disk),
				fmt.Sprintf("active    %d requests, %d ssh sessions", payload.ActiveRequests, payload.ActiveSSHSessions),
				fmt.Sprintf("traffic   %d local, %d passthrough, %d hydrations, %d pack hits, %d rejected",
					payload.Totals.LocalRequests, payload.Totals.PassthroughRequests, payload.Totals.HydratedRepositories, payload.Totals.PackCacheHits, payload.Totals.RejectedRequests),
			} {
				fmt.Fprintln(out, line)
			}
			return nil
		},
	}
	addAdminFlags(cmd, &addr, &timeout, &asJSON)
	return cmd
}
