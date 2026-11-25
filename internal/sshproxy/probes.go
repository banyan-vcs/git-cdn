package sshproxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"gitlab.com/banyan-vcs/git-cdn/internal/logging"
	"gitlab.com/banyan-vcs/git-cdn/internal/protocol"
)

// probeFlights merges the origin ref lookups that authorize local fetches.
type probeFlights struct {
	mu      sync.Mutex
	flights map[string]*probeFlight
}

// probeFlight carries one in-flight lookup's outcome to its waiters.
type probeFlight struct {
	done chan struct{}
	refs protocol.Snapshot
	err  error
}

// sharedRefs resolves the live origin snapshot for a fetch. Concurrent
// lookups for the same origin, repository and password share one SSH
// connection: every waiter receives the leader's fresh result for the same
// instant, so authorization stays live while a fleet cloning one repository
// costs a single probe instead of one per session. The password enters the
// key only as a hash.
func (s *Server) sharedRefs(ctx context.Context, o *origin, password []byte, timeout time.Duration) (protocol.Snapshot, error) {
	digest := sha256.Sum256(password)
	key := o.settings.Address + "\x00" + o.user + "\x00" + o.repo + "\x00" + hex.EncodeToString(digest[:])
	c := &s.probes
	c.mu.Lock()
	if flight, ok := c.flights[key]; ok {
		c.mu.Unlock()
		select {
		case <-flight.done:
			logging.Logger(ctx, s.logger).DebugContext(ctx, "SSH refs probe merged", "upstream_host", o.settings.Address, "repository", o.repo)
			return flight.refs, flight.err
		case <-ctx.Done():
			return protocol.Snapshot{}, ctx.Err()
		}
	}
	flight := &probeFlight{done: make(chan struct{})}
	if c.flights == nil {
		c.flights = make(map[string]*probeFlight)
	}
	c.flights[key] = flight
	c.mu.Unlock()
	// The shared probe must not die with the leader's session: waiters
	// still need its result after the leader disconnects.
	probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	refs, err := o.refs(probeCtx, password, s.maxRefs)
	flight.refs, flight.err = refs, err
	close(flight.done)
	c.mu.Lock()
	if c.flights[key] == flight {
		delete(c.flights, key)
	}
	c.mu.Unlock()
	return refs, err
}

// authThrottle stops a source address whose passwords the origin keeps
// rejecting. The origin sees every attempt as coming from this proxy, so a
// brute-force run through it would trip the origin's lockout for the proxy's
// address and every user behind it. Here the run costs the caller its
// access instead: authFailureLimit rejections within authFailureWindow close
// the address's connections for authBlockDuration without reaching the
// origin, and a connection attempted during a block restarts the block. A
// successful authentication forgets the address's failures, so mistyped
// passwords do not accumulate.
type authThrottle struct {
	mu       sync.Mutex
	failures map[string]*authFailures
}

type authFailures struct {
	count        int
	window       time.Time
	blockedUntil time.Time
}

const (
	authFailureWindow  = time.Minute
	authFailureLimit   = 20
	authBlockDuration  = 5 * time.Minute
	authFailureSources = 4096
)

func (t *authThrottle) record(peer string) {
	if peer == "" {
		return
	}
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.failures == nil {
		t.failures = make(map[string]*authFailures)
	}
	entry := t.failures[peer]
	if entry == nil {
		if len(t.failures) >= authFailureSources {
			// Addresses past their window and block are forgotten first;
			// only a flood of distinct sources drops a live one, which
			// costs at most one extra attempt before the limit again.
			for key, other := range t.failures {
				if !now.Before(other.blockedUntil) && now.Sub(other.window) > authFailureWindow {
					delete(t.failures, key)
				}
			}
			for key := range t.failures {
				if len(t.failures) < authFailureSources {
					break
				}
				delete(t.failures, key)
			}
		}
		entry = &authFailures{window: now}
		t.failures[peer] = entry
	}
	if now.Sub(entry.window) > authFailureWindow {
		entry.window, entry.count = now, 0
	}
	entry.count++
	if entry.count >= authFailureLimit {
		entry.blockedUntil = now.Add(authBlockDuration)
	}
}

// blocked reports whether peer is refused and, when it is, restarts its
// block: an address that keeps trying stays out.
func (t *authThrottle) blocked(peer string) bool {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.failures[peer]
	if entry == nil {
		return false
	}
	if now.Before(entry.blockedUntil) {
		entry.blockedUntil = now.Add(authBlockDuration)
		return true
	}
	if !entry.blockedUntil.IsZero() || now.Sub(entry.window) > authFailureWindow {
		delete(t.failures, peer)
	}
	return false
}

func (t *authThrottle) accepted(peer string) {
	t.mu.Lock()
	delete(t.failures, peer)
	t.mu.Unlock()
}
