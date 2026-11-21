package cache

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSharedLeasesRunConcurrently(t *testing.T) {
	s := testStore(t, 1<<20)
	first, err := s.AcquireShared(context.Background(), s.Key("a"))
	require.NoError(t, err)
	second, err := s.AcquireShared(context.Background(), s.Key("a"))
	require.NoError(t, err, "a second reader could not join the shared gate")
	second.Release()
	first.Release()
}

func TestExclusiveWaitsForReaders(t *testing.T) {
	s := testStore(t, 1<<20)
	reader, err := s.AcquireShared(context.Background(), s.Key("a"))
	require.NoError(t, err)
	acquired := make(chan *Lease, 1)
	go func() {
		l, err := s.Acquire(context.Background(), s.Key("a"))
		if err != nil {
			close(acquired)
			return
		}
		acquired <- l
	}()
	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.entries[s.Key("a")].writers == 1
	}, 5*time.Second, 5*time.Millisecond, "writer did not register behind the reader")
	select {
	case l := <-acquired:
		if l != nil {
			l.Release()
		}
		require.FailNow(t, "test failed", "exclusive lease barged past an active reader")
	default:
	}
	reader.Release()
	select {
	case writer := <-acquired:
		require.NotNil(t, writer, "writer failed while waiting")
		writer.Release()
	case <-time.After(5 * time.Second):
		require.FailNow(t, "test failed", "exclusive lease did not acquire after readers left")
	}
}

func TestWaitingWriterBlocksNewReaders(t *testing.T) {
	s := testStore(t, 1<<20)
	reader, err := s.AcquireShared(context.Background(), s.Key("a"))
	require.NoError(t, err)
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		l, err := s.Acquire(context.Background(), s.Key("a"))
		if err != nil {
			return
		}
		l.Release()
	}()
	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.entries[s.Key("a")].writers == 1
	}, 5*time.Second, 5*time.Millisecond, "writer did not register behind the reader")
	blocked := make(chan *Lease, 1)
	go func() {
		l, err := s.AcquireShared(context.Background(), s.Key("a"))
		if err != nil {
			close(blocked)
			return
		}
		blocked <- l
	}()
	select {
	case l := <-blocked:
		if l != nil {
			l.Release()
		}
		require.FailNow(t, "test failed", "a fresh reader barged past a waiting writer")
	case <-time.After(100 * time.Millisecond):
	}
	reader.Release()
	select {
	case l := <-blocked:
		require.NotNil(t, l, "fresh reader failed behind the writer")
		l.Release()
	case <-time.After(5 * time.Second):
		require.FailNow(t, "test failed", "the fresh reader never acquired after the writer finished")
	}
	<-writerDone
}

func TestGateWaiterContextCancelUnpinsEntry(t *testing.T) {
	s := testStore(t, 1<<20)
	holder, err := s.Acquire(context.Background(), s.Key("a"))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	waitErr := make(chan error, 1)
	go func() {
		_, err := s.Acquire(ctx, s.Key("a"))
		waitErr <- err
	}()
	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.entries[s.Key("a")].writers == 1
	}, 5*time.Second, 5*time.Millisecond, "waiter did not register")
	cancel()
	select {
	case err := <-waitErr:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "test failed", "cancelled waiter did not return")
	}
	s.mu.Lock()
	e := s.entries[s.Key("a")]
	users := e.users
	writers := e.writers
	s.mu.Unlock()
	require.EqualValues(t, 1, users, "cancelled waiter kept the entry pinned")
	require.EqualValues(t, 0, writers, "cancelled waiter kept its writer reservation")
	holder.Release()
}

func TestWaitingRequestPreemptsMaintenance(t *testing.T) {
	s := testStore(t, 1<<20)
	e := newEntry(s.Key("a"))
	s.mu.Lock()
	s.entries[e.key] = e
	e.users++
	e.writer = true
	cancelled := make(chan struct{})
	e.cancelMaint = func() { close(cancelled) }
	s.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		_, err := s.AcquireShared(context.Background(), e.key)
		done <- err
	}()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "test failed", "waiting reader did not preempt maintenance")
	}
	// Maintenance observes the cancellation and leaves the gate.
	s.mu.Lock()
	e.cancelMaint = nil
	e.writer = false
	e.users--
	e.signalGate()
	s.mu.Unlock()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "test failed", "reader did not acquire after maintenance left")
	}
}
