package logging

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/stretchr/testify/require"
	"log/slog"
	"net"
	"net/url"
	"syscall"
	"testing"
)

func TestErrorFieldsDoNotExposeInputData(t *testing.T) {
	err := &url.Error{Op: "Get", URL: "https://user:private-password@example.test/repo?token=private-token", Err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}}
	attrs := ErrorAttrs(err)
	values := map[string]any{}
	for _, a := range attrs {
		values[a.Key] = a.Value.Any()
	}
	require.Equal(t, "dial", values["network_op"])
	require.EqualValues(t, syscall.ECONNREFUSED, values["errno"])
	for _, err := range []error{err, errors.New("private-token and upstream stderr")} {
		fields := ErrorAttrs(err)
		out := map[string]any{}
		for _, a := range fields {
			out[a.Key] = a.Value.Any()
		}
		data, e := json.Marshal(out)
		require.NoError(t, e)
		require.NotContains(t, string(data), "private-token")
		require.NotContains(t, string(data), "private-password")
		require.NotContains(t, string(data), "upstream stderr")
	}
	for _, err := range []error{context.Canceled, context.DeadlineExceeded} {
		require.NotEmpty(t, ErrorAttrs(err))
	}
	require.Empty(t, ErrorAttrs(nil))
}
func TestLoggerContextRetainsParent(t *testing.T) {
	type key struct{}
	base := context.WithValue(context.Background(), key{}, "request")
	logger := slog.Default().With("repository", "Group/Repo.git")
	ctx := WithLogger(base, logger)
	require.Same(t, logger, Logger(ctx, nil))
	require.Equal(t, "request", ctx.Value(key{}))
}
