package sshproxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

func lfsOID(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

type lfsObject struct {
	OID           string                    `json:"oid"`
	Size          int64                     `json:"size"`
	Authenticated bool                      `json:"authenticated,omitempty"`
	Actions       map[string]map[string]any `json:"actions,omitempty"`
}

type lfsHTTPFixture struct {
	api, objects       *httptest.Server
	mu                 sync.Mutex
	data               map[string][]byte
	downloads, uploads atomic.Int64
}

func newLFSHTTPFixture(t *testing.T, initial []byte) *lfsHTTPFixture {
	t.Helper()
	f := &lfsHTTPFixture{data: map[string][]byte{lfsOID(initial): initial}}
	f.objects = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		oid := strings.TrimPrefix(r.URL.Path, "/objects/")
		if r.URL.Query().Get("sig") != "a+b/c" {
			http.Error(w, "invalid signature", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodGet && r.Header.Get("X-LFS-Object-Token") == "object-download" {
			f.mu.Lock()
			data, exists := f.data[oid]
			f.mu.Unlock()
			if !exists {
				http.NotFound(w, r)
				return
			}
			f.downloads.Add(1)
			w.Write(data)
			return
		}
		if r.Method == http.MethodPut && r.Header.Get("X-LFS-Object-Token") == "object-upload" {
			data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
			if err != nil || lfsOID(data) != oid {
				http.Error(w, "invalid object", http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			f.data[oid] = data
			f.mu.Unlock()
			f.uploads.Add(1)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "invalid object authorization", http.StatusForbidden)
	}))
	f.api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var batch struct {
			Operation string      `json:"operation"`
			Objects   []lfsObject `json:"objects"`
		}
		if r.Method != http.MethodPost || r.URL.Path != "/Group/Repo.git/info/lfs/objects/batch" || json.NewDecoder(r.Body).Decode(&batch) != nil {
			http.Error(w, "invalid batch", http.StatusBadRequest)
			return
		}
		if (batch.Operation != "upload" && batch.Operation != "download") || r.Header.Get("Authorization") != "Bearer lfs-"+batch.Operation {
			http.Error(w, "invalid batch authorization", http.StatusForbidden)
			return
		}
		for i := range batch.Objects {
			object := &batch.Objects[i]
			object.Authenticated = true
			object.Actions = map[string]map[string]any{batch.Operation: {
				"href":   f.objects.URL + "/objects/" + object.OID + "?sig=a%2Bb%2Fc",
				"header": map[string]string{"X-LFS-Object-Token": "object-" + batch.Operation},
			}}
		}
		w.Header().Set("Content-Type", "application/vnd.git-lfs+json")
		json.NewEncoder(w).Encode(map[string]any{"transfer": "basic", "objects": batch.Objects})
	}))
	t.Cleanup(f.api.Close)
	t.Cleanup(f.objects.Close)
	return f
}

func TestNativeLFSClonePushAndExternalObjectURLs(t *testing.T) {
	if _, err := exec.LookPath("git-lfs"); err != nil {
		t.Skip("native Git LFS client is required")
	}
	initial := bytes.Repeat([]byte("binary LFS payload\x00\xff\n"), 4096)
	lfs := newLFSHTTPFixture(t, initial)
	var authenticate, transferAttempts atomic.Int64
	f := newSSHFixtureWithLFS(t, func(ch ssh.Channel, command gitCommand) uint32 {
		if command.name == "git-lfs-transfer" {
			transferAttempts.Add(1)
			return 1 // This origin supports hybrid LFS, so the client must fall back.
		}
		if command.name != "git-lfs-authenticate" || command.repo != "Group/Repo.git" {
			return 1
		}
		authenticate.Add(1)
		json.NewEncoder(ch).Encode(map[string]any{
			"href":       lfs.api.URL + "/Group/Repo.git/info/lfs",
			"header":     map[string]string{"Authorization": "Bearer lfs-" + command.operation},
			"expires_in": 300,
		})
		return 0
	})
	pointer := fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", lfsOID(initial), len(initial))
	require.NoError(t, os.WriteFile(filepath.Join(f.work, "asset.bin"), []byte(pointer), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(f.work, ".gitattributes"), []byte("*.bin filter=lfs diff=lfs merge=lfs -text\n"), 0600))
	gitFixture(t, f.work, "add", "asset.bin", ".gitattributes")
	gitFixture(t, f.work, "commit", "-q", "-m", "LFS pointer")
	gitFixture(t, f.work, "push", "-q", f.repo, "main")
	clone := func(dir string) {
		f.git(t, "", "clone", "--quiet", "-c", "filter.lfs.process=git-lfs filter-process", "-c", "filter.lfs.required=true", "-c", "lfs.sshtransfer=negotiate", f.base, dir)
	}
	client := filepath.Join(t.TempDir(), "client")
	clone(client)
	data, err := os.ReadFile(filepath.Join(client, "asset.bin"))
	require.NoError(t, err)
	require.Equal(t, initial, data)
	require.Equal(t, strings.TrimSpace(pointer), gitFixture(t, f.repo, "show", "HEAD:asset.bin"))
	gitFixture(t, client, "lfs", "install", "--local")
	gitFixture(t, client, "config", "lfs.locksverify", "false")
	updated := bytes.Repeat([]byte("updated LFS payload\x00\xfe\n"), 4096)
	require.NoError(t, os.WriteFile(filepath.Join(client, "asset.bin"), updated, 0600))
	gitFixture(t, client, "add", "asset.bin")
	gitFixture(t, client, "commit", "-q", "-m", "Update LFS object")
	f.git(t, client, "push", "--quiet", "origin", "main")
	require.Greater(t, lfs.uploads.Load(), int64(0))
	lfs.mu.Lock()
	uploaded := lfs.data[lfsOID(updated)]
	lfs.mu.Unlock()
	require.Equal(t, updated, uploaded)
	second := filepath.Join(t.TempDir(), "second")
	clone(second)
	data, err = os.ReadFile(filepath.Join(second, "asset.bin"))
	require.NoError(t, err)
	require.Equal(t, updated, data)
	require.GreaterOrEqual(t, lfs.downloads.Load(), int64(2))
	require.GreaterOrEqual(t, authenticate.Load(), int64(3))
	require.Greater(t, transferAttempts.Load(), int64(0))
	local, _, hydration := f.server.Stats()
	require.EqualValues(t, 2, local)
	require.EqualValues(t, 2, hydration)
}
