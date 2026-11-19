//go:build !linux && !darwin

package sshproxy

import (
	"context"
	"fmt"
	"os"
)

func lockHostKeys(context.Context, string) (*os.File, error) {
	return nil, fmt.Errorf("SSH host-key enrollment requires Linux or macOS")
}
