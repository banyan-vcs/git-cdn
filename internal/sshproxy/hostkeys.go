package sshproxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/chiyutianyi/git-cdn/internal/config"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

var errHostKeyCheck = errors.New("origin host key verification failed")

func loadHostKeys(path string, allowMissing bool) (ssh.HostKeyCallback, error) {
	info, err := os.Stat(path)
	if allowMissing && errors.Is(err, os.ErrNotExist) {
		return func(string, net.Addr, ssh.PublicKey) error { return &knownhosts.KeyError{} }, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("known_hosts must be a regular file")
	}
	return knownhosts.New(path)
}

func checkHostKey(path string, allowMissing bool, address string, remote net.Addr, key ssh.PublicKey) error {
	check, err := loadHostKeys(path, allowMissing)
	if err != nil {
		return err
	}
	return check(address, remote, key)
}

func unknownHostKey(err error) bool {
	var keyError *knownhosts.KeyError
	return errors.As(err, &keyError) && len(keyError.Want) == 0
}

func verifyHostKey(ctx context.Context, settings config.SSHUpstream, address string, remote net.Addr, key ssh.PublicKey) error {
	acceptNew := settings.HostKeyPolicy == config.SSHHostKeyAcceptNew
	err := checkHostKey(settings.KnownHosts, acceptNew, address, remote, key)
	if err == nil || !acceptNew || !unknownHostKey(err) {
		return err
	}
	// Host certificates require a trusted CA; do not pin an unverified certificate.
	if _, certificate := key.(*ssh.Certificate); certificate {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(settings.KnownHosts), 0700); err != nil {
		return err
	}
	lock, err := lockHostKeys(ctx, settings.KnownHosts+".lock")
	if err != nil {
		return err
	}
	defer lock.Close()
	// A different connection or process may have pinned this host while we waited.
	err = checkHostKey(settings.KnownHosts, true, address, remote, key)
	if err == nil || !unknownHostKey(err) {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return persistHostKey(settings.KnownHosts, address, key)
}

// Persist before returning from the host-key callback, so a failed write never
// allows SSH to submit a password to an unrecorded destination.
func persistHostKey(path, address string, key ssh.PublicKey) error {
	mode := os.FileMode(0600)
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("known_hosts must be a regular file for enrollment")
		}
		mode = info.Mode().Perm()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}
	data = append(data, knownhosts.Line([]string{knownhosts.Normalize(address)}, key)...)
	data = append(data, '\n')
	file, err := os.CreateTemp(filepath.Dir(path), ".known_hosts-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := file.Chmod(mode); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
