package config

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestSSHConfigurationDefaultsAndValidation(t *testing.T) {
	body := "mode: single\nupstream:\n  url: https://git.example.com\nssh:\n  host_key: ~/ssh-key\n  upstream:\n    address: git.example.com:22\n    known_hosts: ~/known_hosts\n"
	c, err := loadYAML(t, body)
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1:2222", c.SSH.Listen)
	require.Equal(t, []string{"git"}, c.SSH.AllowedUsers)
	for _, suffix := range []string{"\n  max_connections: -1\n", "\n  allowed_users: ['user with spaces']\n", "\n  upstreams: {}\n", "\n  mode: unknown\n"} {
		_, err := loadYAML(t, body+suffix)
		require.Error(t, err)
	}
	for _, target := range []SSHUpstream{{Address: "host:22"}, {Address: "host:0", KnownHosts: "known"}, {Address: "host:22", KnownHosts: "known", PathPrefix: "../repo"}, {Address: "host:22", KnownHosts: "known", User: "-option"}} {
		s := SSH{HostKey: "key", Upstream: &target}
		require.Error(t, s.Validate())
	}
}
