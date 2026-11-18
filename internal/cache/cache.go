package cache

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/chiyutianyi/git-cdn/internal/nativegit"
)

type Store struct {
	Root          string
	Tmp           string
	Git           *nativegit.Runner
	secret        []byte
	lock          *os.File
	mu            sync.Mutex
	entries       map[string]*entry
	maxBytes      int64
	usedBytes     int64
	reservedBytes int64
	evictions     uint64
	evictedBytes  uint64
}

func Open(root string, git *nativegit.Runner, maxBytes int64) (*Store, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("cache byte limit must be positive")
	}
	root, err := filepath.Abs(root)
	if err != nil || strings.ContainsAny(root, "\r\n") {
		return nil, fmt.Errorf("invalid cache directory")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	lock, err := lockDirectory(filepath.Join(root, ".lock"))
	if err != nil {
		return nil, err
	}
	s := &Store{Root: root, Tmp: filepath.Join(root, "tmp"), Git: git, lock: lock, entries: make(map[string]*entry), maxBytes: maxBytes}
	if err = ensureDirectory(s.Tmp); err != nil {
		s.Close()
		return nil, err
	}
	secretPath := filepath.Join(root, ".scope-key")
	s.secret, err = os.ReadFile(secretPath)
	if os.IsNotExist(err) {
		s.secret = make([]byte, 32)
		if _, err = rand.Read(s.secret); err == nil {
			err = os.WriteFile(secretPath, s.secret, 0600)
		}
	}
	if err != nil || len(s.secret) != 32 {
		s.Close()
		return nil, fmt.Errorf("cannot initialize credential cache scope")
	}
	if err := s.loadEntries(); err != nil {
		s.Close()
		return nil, err
	}
	if err := s.makeRoomLocked(0); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.lock.Close()
}

func (s *Store) Key(identity, credential string) string {
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte(credential))
	identityHash := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(identityHash[:]) + "/" + hex.EncodeToString(m.Sum(nil))
}

type Lease struct {
	Dir   string
	store *Store
	entry *entry
}

func (s *Store) Acquire(ctx context.Context, key string) (*Lease, error) {
	s.mu.Lock()
	if !validKey(key) {
		s.mu.Unlock()
		return nil, fmt.Errorf("invalid cache key")
	}
	e := s.entries[key]
	if e == nil {
		e = &entry{key: key, gate: make(chan struct{}, 1)}
		s.entries[key] = e
	}
	// Waiting callers also pin the entry so eviction cannot replace their gate.
	e.users++
	s.mu.Unlock()
	select {
	case e.gate <- struct{}{}:
		s.mu.Lock()
		e.accessed = time.Now()
		s.mu.Unlock()
		return &Lease{Dir: filepath.Join(s.Root, "repos", key, "repo.git"), store: s, entry: e}, nil
	case <-ctx.Done():
		s.mu.Lock()
		e.users--
		if e.users == 0 && e.size == 0 {
			delete(s.entries, e.key)
		}
		s.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (l *Lease) Release() {
	s := l.store
	s.mu.Lock()
	e := l.entry
	e.accessed = time.Now()
	if size, err := directorySize(filepath.Dir(l.Dir)); err == nil {
		s.usedBytes += size - e.size
		e.size = size
	}
	if e.size > 0 {
		access := filepath.Join(filepath.Dir(l.Dir), ".access")
		if err := os.WriteFile(access, nil, 0600); err == nil {
			_ = os.Chtimes(access, e.accessed, e.accessed)
		}
	}
	e.users--
	<-e.gate
	// Any overage from interrupted metadata writes is reclaimed when leases end.
	_ = s.makeRoomLocked(0)
	if e.users == 0 && e.size == 0 {
		delete(s.entries, e.key)
		_ = os.Remove(filepath.Dir(l.Dir))
		_ = os.Remove(filepath.Dir(filepath.Dir(l.Dir)))
	}
	s.mu.Unlock()
}
