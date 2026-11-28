package cache

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gitlab.com/banyan-vcs/git-cdn/internal/protocol"
)

// Migration holds the directory lock. Objects move before roots, and old scope
// metadata remains until the merged repository validates, so startup can retry.
func (s *Store) migrateLegacy() error {
	repos := filepath.Join(s.Root, "repos")
	if err := ensureDirectory(repos); err != nil {
		return err
	}
	identities, err := os.ReadDir(repos)
	if err != nil {
		return err
	}
	for _, identity := range identities {
		if !identity.IsDir() || !digest(identity.Name()) {
			continue
		}
		parent := filepath.Join(repos, identity.Name())
		children, err := os.ReadDir(parent)
		if err != nil {
			return err
		}
		var scopes []string
		for _, child := range children {
			if child.IsDir() && digest(child.Name()) {
				scopes = append(scopes, filepath.Join(parent, child.Name()))
			}
		}
		if len(scopes) == 0 {
			continue
		}
		if s.Git == nil {
			return fmt.Errorf("native Git is required to migrate legacy caches")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		err = s.mergeLegacy(ctx, parent, scopes)
		cancel()
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) mergeLegacy(ctx context.Context, parent string, scopes []string) error {
	repo := filepath.Join(parent, "repo.git")
	if _, err := os.Stat(filepath.Join(repo, "HEAD")); os.IsNotExist(err) {
		if err := s.Git.InitBare(ctx, repo); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	var updates strings.Builder
	var accessed time.Time
	for _, scope := range scopes {
		source := filepath.Join(scope, "repo.git")
		if _, err := os.Stat(filepath.Join(source, "HEAD")); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		journal := filepath.Join(scope, ".migration-roots")
		refs, err := os.ReadFile(journal)
		if os.IsNotExist(err) {
			refs, err = s.Git.Output(ctx, source, "", "for-each-ref", "--format=%(refname) %(objectname)", "refs/cache/roots/")
			if err == nil {
				err = os.WriteFile(journal, refs, 0600)
			}
		}
		if err != nil {
			return err
		}
		for _, line := range strings.Split(strings.TrimSpace(string(refs)), "\n") {
			if line == "" {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) != 2 || !protocol.RefName(fields[0]) || !protocol.OID(fields[1]) {
				return fmt.Errorf("invalid legacy cache root")
			}
			fmt.Fprintf(&updates, "update %s %s\n", fields[0], fields[1])
		}
		objects := filepath.Join(source, "objects")
		if err := filepath.WalkDir(objects, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("legacy objects cannot contain symlinks")
			}
			if entry.IsDir() {
				return nil
			}
			relative, err := filepath.Rel(objects, path)
			if err != nil {
				return err
			}
			parts := strings.Split(filepath.ToSlash(relative), "/")
			// Import immutable object data; ancillary indexes can be rebuilt.
			if len(parts) != 2 || (parts[0] != "pack" && len(parts[0]) != 2) {
				return nil
			}
			if parts[0] == "pack" && !(strings.HasSuffix(path, ".pack") || strings.HasSuffix(path, ".idx") || strings.HasSuffix(path, ".rev") || strings.HasSuffix(path, ".mtimes")) {
				return nil
			}
			target := filepath.Join(repo, "objects", relative)
			if err := ensureDirectory(filepath.Dir(target)); err != nil {
				return err
			}
			if _, err := os.Stat(target); err == nil {
				return nil
			} else if !os.IsNotExist(err) {
				return err
			}
			return os.Rename(path, target)
		}); err != nil {
			return err
		}
		if info, err := os.Stat(filepath.Join(scope, ".access")); err == nil && info.ModTime().After(accessed) {
			accessed = info.ModTime()
		}
	}
	// Duplicate roots across credentials must appear only once in the transaction.
	unique := make(map[string]bool)
	var transaction strings.Builder
	var writes []string
	for _, line := range strings.Split(updates.String(), "\n") {
		if line != "" && !unique[line] {
			unique[line] = true
			transaction.WriteString(line + "\n")
			writes = append(writes, strings.Fields(line)[1])
		}
	}
	if transaction.Len() > 0 {
		// The previous attempt may have been stopped inside its transaction.
		if err := clearStaleRefWrites(repo, writes); err != nil {
			return err
		}
		if err := s.Git.Run(ctx, repo, strings.NewReader(transaction.String()), io.Discard, nil, "update-ref", "--stdin"); err != nil {
			return err
		}
	}
	if err := s.Git.Run(ctx, repo, nil, io.Discard, nil, "rev-list", "--objects", "--missing=error", "--quiet", "--all"); err != nil {
		return err
	}
	access := filepath.Join(parent, ".access")
	if err := os.WriteFile(access, nil, 0600); err != nil {
		return err
	}
	if !accessed.IsZero() {
		if err := os.Chtimes(access, accessed, accessed); err != nil {
			return err
		}
	}
	for _, scope := range scopes {
		if err := os.RemoveAll(scope); err != nil {
			return err
		}
	}
	return nil
}
