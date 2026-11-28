package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

type ackPayload struct {
	Status            string `json:"status"`
	ActiveRequests    int    `json:"active_requests"`
	ActiveSSHSessions int    `json:"active_ssh_sessions"`
}

func newDrainCommand() *cobra.Command {
	var (
		addr        string
		timeout     time.Duration
		asJSON      bool
		wait        bool
		waitTimeout time.Duration
	)
	cmd := &cobra.Command{
		Use:   "drain",
		Short: "Stop admitting new requests on the node",
		Long: "Mark the node draining: new git requests are refused on both\n" +
			"frontends and /healthz reports the drain, so load balancers stop\n" +
			"routing here while in-flight transfers finish. Long SSH sessions\n" +
			"may outlive --wait-timeout; check status afterwards.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newAdminClient(addr, timeout)
			raw, err := client.call("POST", "/drain", nil)
			if err != nil {
				return err
			}
			if asJSON {
				_, err = cmd.OutOrStdout().Write(raw)
				return err
			}
			var ack ackPayload
			if err := json.Unmarshal(raw, &ack); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "draining: %d requests, %d ssh sessions in flight\n", ack.ActiveRequests, ack.ActiveSSHSessions)
			if !wait {
				return nil
			}
			if err := waitForQuiet(cmd, client, waitTimeout); err != nil {
				return err
			}
			fmt.Fprintln(out, "node is quiet")
			return nil
		},
	}
	addAdminFlags(cmd, &addr, &timeout, &asJSON)
	cmd.Flags().BoolVar(&wait, "wait", false, "Wait until in-flight requests and sessions reach zero")
	cmd.Flags().DurationVar(&waitTimeout, "wait-timeout", time.Minute, "Give up waiting after this long")
	return cmd
}

func newUndrainCommand() *cobra.Command {
	var (
		addr    string
		timeout time.Duration
		asJSON  bool
	)
	cmd := &cobra.Command{
		Use:   "undrain",
		Short: "Resume admitting requests on the node",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newAdminClient(addr, timeout)
			raw, err := client.call("POST", "/undrain", nil)
			if err != nil {
				return err
			}
			if asJSON {
				_, err = cmd.OutOrStdout().Write(raw)
				return err
			}
			var ack ackPayload
			if err := json.Unmarshal(raw, &ack); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "ok: %d requests, %d ssh sessions in flight\n", ack.ActiveRequests, ack.ActiveSSHSessions)
			return nil
		},
	}
	addAdminFlags(cmd, &addr, &timeout, &asJSON)
	return cmd
}

// waitForQuiet polls status until both frontends hold no in-flight work.
func waitForQuiet(cmd *cobra.Command, client *adminClient, waitTimeout time.Duration) error {
	deadline := time.Now().Add(waitTimeout)
	for {
		var payload struct {
			ActiveRequests    int `json:"active_requests"`
			ActiveSSHSessions int `json:"active_ssh_sessions"`
		}
		if _, err := client.call("GET", "/status", &payload); err != nil {
			return err
		}
		if payload.ActiveRequests == 0 && payload.ActiveSSHSessions == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("still draining: %d requests, %d ssh sessions in flight after %s",
				payload.ActiveRequests, payload.ActiveSSHSessions, waitTimeout)
		}
		select {
		case <-time.After(500 * time.Millisecond):
		case <-cmd.Context().Done():
			return cmd.Context().Err()
		}
	}
}
