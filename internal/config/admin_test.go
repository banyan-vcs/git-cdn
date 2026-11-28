package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// admin_listen is tri-state: absent takes the loopback default, an empty
// string disables the endpoint, and any other value must stay on loopback
// because the endpoint carries no authentication yet.
func TestAdminListenValidation(t *testing.T) {
	base := "mode: multi\nupstreams:\n  gitlab:\n    url: https://git.example.com\n"

	c, err := loadYAML(t, base)
	require.NoError(t, err)
	require.NotNil(t, c.AdminListen)
	require.Equal(t, "127.0.0.1:8081", *c.AdminListen, "the loopback default was not applied")

	c, err = loadYAML(t, base+"admin_listen: \"\"\n")
	require.NoError(t, err)
	require.NotNil(t, c.AdminListen)
	require.Empty(t, *c.AdminListen, "an explicit empty string did not disable the endpoint")

	for _, listen := range []string{"127.0.0.1:9090", "localhost:9090", "[::1]:9090"} {
		c, err = loadYAML(t, base+"admin_listen: \""+listen+"\"\n")
		require.NoError(t, err, listen)
		require.Equal(t, listen, *c.AdminListen)
	}

	for _, listen := range []string{"0.0.0.0:9090", "192.168.0.2:9090", "example.com:9090", "127.0.0.1", "127.0.0.1:notaport"} {
		_, err = loadYAML(t, base+"admin_listen: \""+listen+"\"\n")
		require.Error(t, err, listen)
		require.Contains(t, err.Error(), "admin_listen", listen)
	}
}
