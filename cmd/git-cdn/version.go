package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"gitlab.com/banyan-vcs/git-cdn/internal/buildinfo"
)

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the build version",
		Args:  cobra.NoArgs,
		Run: func(*cobra.Command, []string) {
			fmt.Println("git-cdn version", buildinfo.Version)
		},
	}
}
