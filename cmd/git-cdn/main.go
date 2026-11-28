// Command git-cdn is the Git caching CDN daemon and its node operations
// CLI. The daemon runs under the serve subcommand; the operation
// subcommands talk to a running node's admin endpoint.
package main

import (
	"os"

	"github.com/spf13/cobra"
)

func main() {
	if err := newRootCommand().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "git-cdn",
		Short: "Git caching CDN daemon and node operations",
		SilenceUsage: true,
	}
	root.AddCommand(newServeCommand())
	root.AddCommand(newVersionCommand())
	return root
}
