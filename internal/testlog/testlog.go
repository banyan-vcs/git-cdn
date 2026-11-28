// Package testlog gives a service under test a logger that prints only
// when the test fails.
package testlog

import (
	"bytes"
	"log/slog"
	"sync"
)

// tail bounds what is kept of the log. A busy test writes a few lines per
// request; the end of the log is where a failure was prepared.
const tail = 64 << 10

// sink keeps the end of the log. Handler implementations serialize their
// own writes, but String may be called from the test goroutine while
// requests are still logging.
type sink struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (s *sink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buffer.Write(p)
	if excess := s.buffer.Len() - tail; excess > 0 {
		s.buffer.Next(excess)
	}
	return len(p), nil
}

func (s *sink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buffer.String()
}

// New returns a logger for t. Its output stays quiet while t passes and is
// printed with t when t fails: the log is where a hydration, a
// reconciliation or a passthrough names its phase and error class, and a
// test that discards it fails with nothing but its own assertion.
func New(t TB) *slog.Logger {
	t.Helper()
	logs := &sink{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("service log:\n%s", logs)
		}
	})
	return slog.New(slog.NewTextHandler(logs, nil))
}

type TB interface {
	Helper()
	Cleanup(func())
	Failed() bool
	Logf(string, ...any)
}
