package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gitlab.com/banyan-vcs/git-cdn/internal/nativegit"
)

type cacheLogBuffer struct {
	sync.Mutex
	bytes.Buffer
}

func (b *cacheLogBuffer) Write(data []byte) (int, error) {
	b.Lock()
	defer b.Unlock()
	return b.Buffer.Write(data)
}

func (b *cacheLogBuffer) events(t *testing.T) []map[string]any {
	t.Helper()
	b.Lock()
	defer b.Unlock()
	var events []map[string]any
	decoder := json.NewDecoder(bytes.NewReader(b.Bytes()))
	for decoder.More() {
		var event map[string]any
		require.NoError(t, decoder.Decode(&event))
		events = append(events, event)
	}
	return events
}

func TestPackInvalidationLogsSizeWithoutPayloadOrPath(t *testing.T) {
	var output cacheLogBuffer
	g, err := nativegit.New("git", 1, nativegit.WithLogger(slog.New(slog.NewJSONHandler(&output, nil))))
	require.NoError(t, err)
	s, err := Open(t.TempDir(), g, 1<<20)
	require.NoError(t, err)
	defer s.Close()
	s.SetPackCacheBytes(1 << 20)
	l := testLease(t, s, "credential-bearing-private-identity")
	payload := []byte("private-packet-payload")
	key := storeTestPack(t, l, payload)
	scope := l.entry.key
	path := filepath.Join(s.packDir(scope), packName(key, int64(len(payload))))
	l.Release()
	require.NoError(t, os.Truncate(path, 4))
	file, err := s.LookupPack(scope, key)
	require.NoError(t, err)
	require.Nil(t, file)
	var invalidations []map[string]any
	for _, event := range output.events(t) {
		if event["msg"] == "response pack cache entry invalidated" {
			invalidations = append(invalidations, event)
		}
	}
	require.Len(t, invalidations, 1)
	require.Equal(t, "WARN", invalidations[0]["level"])
	require.Equal(t, scope, invalidations[0]["cache_key"])
	require.EqualValues(t, len(payload), invalidations[0]["expected_bytes"])
	require.EqualValues(t, 4, invalidations[0]["actual_bytes"])
	output.Lock()
	defer output.Unlock()
	require.NotContains(t, output.String(), path)
	require.NotContains(t, output.String(), string(payload))
	require.NotContains(t, output.String(), "credential-bearing-private-identity")
}

func TestReconciliationLogsRetirementAndCancellation(t *testing.T) {
	var output cacheLogBuffer
	logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
	setup := reconcileFixture(t, nativegit.WithLogger(logger))
	l, err := setup.store.Acquire(context.Background(), setup.key)
	require.NoError(t, err)
	require.NoError(t, l.SyncRefs(context.Background(), setup.snapshotFor(setup.first)))
	_, err = l.Ensure(context.Background(), setup.origin, nil, []string{setup.second})
	require.NoError(t, err)
	l.Release()
	setup.store.Reconcile(setup.key, setup.snapshotFor(setup.second))
	require.Eventually(t, func() bool { return setup.store.Stats().RootsRetired > 0 }, 10*time.Second, 20*time.Millisecond)
	l, err = setup.store.Acquire(context.Background(), setup.key)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, l.SyncRefs(ctx, setup.snapshotFor(setup.second)), context.Canceled)
	l.Release()
	require.NoError(t, setup.store.Close())
	var retired, canceled bool
	for _, event := range output.events(t) {
		if event["msg"] == "cache refs reconciled" && event["operation"] == "reconcile" && event["roots_retired"].(float64) > 0 {
			retired = true
			require.Equal(t, setup.key, event["cache_key"])
			require.Equal(t, true, event["changed"])
		}
		if event["msg"] == "cache refs reconciliation failed" && event["error_kind"] == "canceled" {
			canceled = true
			require.Equal(t, "DEBUG", event["level"])
		}
	}
	require.True(t, retired, "background root retirement was not logged")
	require.True(t, canceled, "preempted reconciliation was not logged at DEBUG")
}
