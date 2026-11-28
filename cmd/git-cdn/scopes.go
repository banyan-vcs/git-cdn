package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

type scopePayload struct {
	Scope      string    `json:"scope"`
	Site       string    `json:"site"`
	Bytes      int64     `json:"bytes"`
	PackBytes  int64     `json:"pack_bytes"`
	Packs      int       `json:"packs"`
	Requests   int64     `json:"requests"`
	LastAccess time.Time `json:"last_access"`
}

func newScopesCommand() *cobra.Command {
	var (
		addr    string
		timeout time.Duration
		asJSON  bool
		sortBy  string
		limit   int
	)
	cmd := &cobra.Command{
		Use:   "scopes",
		Short: "List cached repository scopes",
		Long: "List the node's cached repository scopes: the scope key is a\n" +
			"directory hash, never a repository name — the node does not track\n" +
			"names by design.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newAdminClient(addr, timeout)
			raw, err := client.call("GET", "/scopes", nil)
			if err != nil {
				return err
			}
			if asJSON {
				_, err = cmd.OutOrStdout().Write(raw)
				return err
			}
			var payload struct {
				Scopes []scopePayload `json:"scopes"`
			}
			if err := json.Unmarshal(raw, &payload); err != nil {
				return err
			}
			scopes := payload.Scopes
			switch sortBy {
			case "access":
				sort.Slice(scopes, func(i, j int) bool { return scopes[i].LastAccess.After(scopes[j].LastAccess) })
			case "bytes":
				sort.Slice(scopes, func(i, j int) bool { return scopes[i].Bytes > scopes[j].Bytes })
			}
			if limit > 0 && len(scopes) > limit {
				scopes = scopes[:limit]
			}
			out := cmd.OutOrStdout()
			if len(scopes) == 0 {
				fmt.Fprintln(out, "no cached scopes")
				return nil
			}
			writer := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
			fmt.Fprintln(writer, "scope\t site\t bytes\t packs\t requests\t last access")
			for _, scope := range scopes {
				key := scope.Scope
				if len(key) > 12 {
					key = key[:12] + "…"
				}
				fmt.Fprintf(writer, "%s\t %s\t %s\t %d\t %d\t %s\n",
					key, scope.Site, formatBytes(scope.Bytes), scope.Packs, scope.Requests, formatAge(time.Since(scope.LastAccess)))
			}
			return writer.Flush()
		},
	}
	addAdminFlags(cmd, &addr, &timeout, &asJSON)
	cmd.Flags().StringVar(&sortBy, "sort", "bytes", "Sort by bytes or access")
	cmd.Flags().IntVar(&limit, "limit", 0, "Show at most N scopes (0 = all)")
	return cmd
}

// formatAge renders recency the way operators read it.
func formatAge(age time.Duration) string {
	switch {
	case age < 0:
		return "now"
	case age < time.Minute:
		return fmt.Sprintf("%.0fs ago", age.Seconds())
	case age < time.Hour:
		return fmt.Sprintf("%.0fm ago", age.Minutes())
	case age < 24*time.Hour:
		return fmt.Sprintf("%.1fh ago", age.Hours())
	default:
		return fmt.Sprintf("%.1fd ago", age.Hours()/24)
	}
}
