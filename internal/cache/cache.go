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

	"github.com/chiyutianyi/git-cdn/internal/nativegit"
)

type Store struct {
	Root   string
	Tmp    string
	Git    *nativegit.Runner
	secret []byte
	lock   *os.File
	mu     sync.Mutex
	gates  map[string]chan struct{}
}

func Open(root string, git *nativegit.Runner) (*Store, error) {
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
	s := &Store{Root: root, Tmp: filepath.Join(root, "tmp"), Git: git, lock: lock, gates: make(map[string]chan struct{})}
	if err = os.MkdirAll(s.Tmp, 0700); err != nil {
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
	gate  chan struct{}
}

func (s *Store) Acquire(ctx context.Context, key string) (*Lease, error) {
	s.mu.Lock()
	gate := s.gates[key]
	if gate == nil {
		gate = make(chan struct{}, 1)
		s.gates[key] = gate
	}
	s.mu.Unlock()
	select {
	case gate <- struct{}{}:
		return &Lease{Dir: filepath.Join(s.Root, "repos", key, "repo.git"), store: s, gate: gate}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (l *Lease) Release() { <-l.gate }
