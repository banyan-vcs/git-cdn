package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gitlab.com/banyan-vcs/git-cdn/internal/protocol"
)

const retentionFile = ".gc-retained"
const gcFile = ".gc-last"
const retainedPrefix = "refs/cache/retained/"

func atomicFile(dir, name string, data []byte) error {
	file, err := os.CreateTemp(dir, name+"-tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), filepath.Join(dir, name))
}

func readGCTime(dir string, fallback time.Time) (time.Time, error) {
	data, err := os.ReadFile(filepath.Join(dir, gcFile))
	if os.IsNotExist(err) {
		return fallback, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	value, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || value <= 0 {
		return time.Time{}, fmt.Errorf("invalid last GC timestamp")
	}
	return time.Unix(value, 0), nil
}

func readRetention(dir string) (map[string]int64, error) {
	values := make(map[string]int64)
	data, err := os.ReadFile(filepath.Join(dir, retentionFile))
	if os.IsNotExist(err) {
		return values, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, fmt.Errorf("invalid retained roots: %w", err)
	}
	if values == nil {
		return nil, fmt.Errorf("retained roots must be an object")
	}
	for oid, stamp := range values {
		if !protocol.OID(oid) || stamp <= 0 {
			return nil, fmt.Errorf("invalid retained root timestamp")
		}
	}
	return values, nil
}

// Grace roots protect whole histories for at least the retention duration from
// their last unlink. Unknown/missing metadata fails conservatively by renewing
// the grace period. Only staged GC removes expired refs.
func (l *Lease) expireRetained(ctx context.Context, dir string, now time.Time) ([]string, error) {
	values, err := readRetention(dir)
	if err != nil {
		return nil, err
	}
	refs, err := l.store.Git.Output(ctx, dir, "", "for-each-ref", "--format=%(refname) %(objectname)", retainedPrefix)
	if err != nil {
		return nil, err
	}
	var expired []string
	var updates strings.Builder
	present := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSpace(string(refs)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != retainedPrefix+fields[1] || !protocol.OID(fields[1]) {
			return nil, fmt.Errorf("invalid retained ref")
		}
		oid := fields[1]
		present[oid] = true
		if values[oid] == 0 {
			values[oid] = now.UnixNano()
		}
		if !now.Before(time.Unix(0, values[oid]).Add(l.store.repack.GCRetention)) {
			fmt.Fprintf(&updates, "delete %s %s\n", fields[0], oid)
			expired = append(expired, oid)
			delete(values, oid)
		}
	}
	for oid := range values {
		if !present[oid] {
			// A timestamp journal may precede an interrupted ref transaction.
			// Recreate its grace pin rather than pruning recent history.
			if now.Before(time.Unix(0, values[oid]).Add(l.store.repack.GCRetention)) {
				fmt.Fprintf(&updates, "update %s%s %s\n", retainedPrefix, oid, oid)
			} else {
				delete(values, oid)
			}
		}
	}
	if updates.Len() > 0 {
		if err := l.store.Git.Run(ctx, dir, strings.NewReader(updates.String()), io.Discard, nil, "update-ref", "--stdin"); err != nil {
			return nil, err
		}
	}
	data, err := json.Marshal(values)
	if err != nil {
		return nil, err
	}
	if err := atomicFile(dir, retentionFile, data); err != nil {
		return nil, err
	}
	return expired, nil
}
