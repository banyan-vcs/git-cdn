package cache

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

var ErrCapacity = errors.New("repository cache capacity unavailable")

type entry struct {
	key            string
	gate           chan struct{}
	users          int
	size           int64
	accessed       time.Time
	repackPending  bool
	refsObservedAt time.Time
	lastGC         time.Time
}

type Stats struct {
	Bytes         int64
	MaxBytes      int64
	ReservedBytes int64
	Entries       int
	Evictions     uint64
	EvictedBytes  uint64
	Repacks       uint64
	RepackFailed  uint64
	RepackSkipped uint64
	GCs           uint64
	GCFailed      uint64
	GCSkipped     uint64
}

func (s *Store) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{Bytes: s.usedBytes, MaxBytes: s.maxBytes, ReservedBytes: s.reservedBytes, Entries: len(s.entries), Evictions: s.evictions, EvictedBytes: s.evictedBytes, Repacks: s.repacks, RepackFailed: s.repackFailed, RepackSkipped: s.repackSkipped, GCs: s.gcs, GCFailed: s.gcFailed, GCSkipped: s.gcSkipped}
}

func digest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func validKey(key string) bool {
	return digest(key)
}

// Count logical regular-file bytes without following links outside a managed cache.
func directorySize(path string) (int64, error) {
	var size int64
	err := filepath.WalkDir(path, func(path string, d fs.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			size += info.Size()
		}
		return nil
	})
	return size, err
}

func (s *Store) loadEntries() error {
	repos := filepath.Join(s.Root, "repos")
	if err := ensureDirectory(repos); err != nil {
		return err
	}
	identities, err := os.ReadDir(repos)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, identity := range identities {
		if !identity.IsDir() || !digest(identity.Name()) {
			continue
		}
		key := identity.Name()
		path := filepath.Join(repos, key)
		size, err := directorySize(path)
		if err != nil {
			return err
		}
		info, err := os.Stat(filepath.Join(path, ".access"))
		if os.IsNotExist(err) {
			info, err = identity.Info()
		}
		if err != nil {
			return err
		}
		lastGC, err := readGCTime(filepath.Join(path, "repo.git"), info.ModTime())
		if err != nil {
			return err
		}
		s.entries[key] = &entry{key: key, gate: make(chan struct{}, 1), size: size, accessed: info.ModTime(), lastGC: lastGC}
		s.usedBytes += size
	}
	return nil
}

func ensureDirectory(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("managed cache path is not a directory")
	}
	return nil
}

func (s *Store) ensureScope(key string) error {
	for _, path := range []string{filepath.Join(s.Root, "repos"), filepath.Join(s.Root, "repos", key)} {
		if err := ensureDirectory(path); err != nil {
			return err
		}
	}
	return nil
}

// Caller holds mu. Only unpinned entries may be removed, including pending waiters.
func (s *Store) makeRoomLocked(additional int64) error {
	if additional < 0 || additional > s.maxBytes {
		return ErrCapacity
	}
	if s.usedBytes+s.reservedBytes <= s.maxBytes-additional {
		return nil
	}
	var candidates []*entry
	for _, e := range s.entries {
		if e.users == 0 {
			candidates = append(candidates, e)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].accessed.Equal(candidates[j].accessed) {
			return candidates[i].key < candidates[j].key
		}
		return candidates[i].accessed.Before(candidates[j].accessed)
	})
	for _, e := range candidates {
		path := filepath.Join(s.Root, "repos", e.key)
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("evict repository cache: %w", err)
		}
		s.usedBytes -= e.size
		s.evictions++
		s.evictedBytes += uint64(e.size)
		delete(s.entries, e.key)
		if s.usedBytes+s.reservedBytes <= s.maxBytes-additional {
			return nil
		}
	}
	return ErrCapacity
}

type reservation struct {
	lease *Lease
	bytes int64
}

func (l *Lease) reserve(additional int64) (*reservation, error) {
	s := l.store
	s.mu.Lock()
	defer s.mu.Unlock()
	additional = max(additional, 0)
	if l.entry.size > s.maxBytes-additional {
		return nil, ErrCapacity
	}
	if err := s.makeRoomLocked(additional); err != nil {
		return nil, err
	}
	s.reservedBytes += additional
	return &reservation{lease: l, bytes: additional}, nil
}

func (r *reservation) release() {
	s := r.lease.store
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reservedBytes -= r.bytes
	if size, err := directorySize(filepath.Dir(r.lease.Dir)); err == nil {
		s.usedBytes += size - r.lease.entry.size
		r.lease.entry.size = size
	} else {
		// Keep the admission estimate charged when the filesystem cannot be measured.
		s.usedBytes += r.bytes
		r.lease.entry.size += r.bytes
	}
}
