package cache

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The admin scopes view lists what the store actually holds, keyed by
// scope hash with the site a repository was last served for.
func TestScopesSnapshotListsServedRepositories(t *testing.T) {
	s := testStore(t, 1<<30)
	s.SetPackCacheBytes(1 << 20)
	require.Empty(t, s.Scopes(), "a fresh store listed scopes")

	l := testLease(t, s, "repo one")
	key := l.entry.key
	storeKeyedPack(t, l, 1, "stored response body")
	l.Release()

	scopes := s.Scopes()
	require.Len(t, scopes, 1)
	require.Equal(t, key, scopes[0].Scope)
	require.Equal(t, UnknownSite, scopes[0].Site, "an unserved scope reported a site")
	require.Positive(t, scopes[0].Bytes)
	require.False(t, scopes[0].LastAccess.IsZero())

	s.Served(key, "gitlab")
	scopes = s.Scopes()
	require.Len(t, scopes, 1)
	require.Equal(t, "gitlab", scopes[0].Site)
	require.Positive(t, scopes[0].Requests, "a served scope counted no requests")
}

// Shrinking the budget at runtime evicts immediately, so a reload observes
// the new ceiling without waiting for the next hydration to make room.
func TestSetMaxBytesShrinksEvictImmediately(t *testing.T) {
	s := testStore(t, 1<<30)
	s.SetPackCacheBytes(1 << 20)
	for _, name := range []string{"first", "second", "third"} {
		l := testLease(t, s, name)
		storeKeyedPack(t, l, 1, "response body for "+name)
		l.Release()
	}
	require.Equal(t, 3, s.Stats().Entries)

	require.NoError(t, s.SetMaxBytes(1))
	require.EqualValues(t, 1, s.Stats().MaxBytes)
	require.Zero(t, s.Stats().Entries, "a shrunk budget kept scopes resident")
	require.Empty(t, s.Scopes())

	require.EqualError(t, s.SetMaxBytes(0), "cache byte limit must be positive")
	require.EqualValues(t, 1, s.Stats().MaxBytes, "a rejected shrink changed the budget")
}
