package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

func newReloadCommand() *cobra.Command {
	var (
		addr    string
		timeout time.Duration
		asJSON  bool
	)
	cmd := &cobra.Command{
		Use:   "reload",
		Short: "Re-read the node's configuration file",
		Long: "Re-read the YAML the node started from and apply the reloadable\n" +
			"subset live: cache budgets, admission limits, request size caps,\n" +
			"timeouts and the auth probe lifetime. Settings fixed at construction\n" +
			"that differ from the file are reported as restart-required; the\n" +
			"report persists across reloads until the node restarts into them.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newAdminClient(addr, timeout)
			raw, err := client.call("POST", "/reload", nil)
			if err != nil {
				return err
			}
			if asJSON {
				_, err = cmd.OutOrStdout().Write(raw)
				return err
			}
			var payload struct {
				Applied []struct {
					Field string `json:"field"`
					Old   string `json:"old"`
					New   string `json:"new"`
				} `json:"applied"`
				RestartRequired []struct {
					Field string `json:"field"`
					Old   string `json:"old"`
					New   string `json:"new"`
				} `json:"restart_required"`
			}
			if err := json.Unmarshal(raw, &payload); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(payload.Applied) == 0 && len(payload.RestartRequired) == 0 {
				fmt.Fprintln(out, "nothing to do")
				return nil
			}
			for _, change := range payload.Applied {
				fmt.Fprintf(out, "applied   %s: %s -> %s\n", change.Field, change.Old, change.New)
			}
			for _, change := range payload.RestartRequired {
				fmt.Fprintf(out, "restart   %s: %s -> %s\n", change.Field, change.Old, change.New)
			}
			return nil
		},
	}
	addAdminFlags(cmd, &addr, &timeout, &asJSON)
	return cmd
}
