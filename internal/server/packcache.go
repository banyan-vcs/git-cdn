package server

import (
	"crypto/sha256"

	"github.com/chiyutianyi/git-cdn/internal/protocol"
)

// packCacheKey derives the verbatim response key for a request. Only
// clone-shaped fetches (done, unfiltered, no haves) participate: their
// request bytes repeat across clients, while incremental fetches embed
// client-specific haves that defeat reuse. The key binds the repository
// identity, the exact decoded request bytes (wants, capabilities, flags as
// upload-pack saw them) and the live ref snapshot; a changed snapshot — new
// tips or new tags, which redirect include-tag output even for identical
// want sets — produces a different key and a fresh generation.
func (s *Server) packCacheKey(identity string, fetch protocol.Fetch, data []byte, snapshot protocol.Snapshot) []byte {
	if s.store.PackCacheBytes() <= 0 || !fetch.FullFetch() {
		return nil
	}
	h := sha256.New()
	h.Write([]byte(identity))
	h.Write([]byte{0})
	h.Write(data)
	h.Write([]byte{0})
	for _, ref := range snapshot.Refs {
		h.Write([]byte(ref.Name))
		h.Write([]byte{0})
		h.Write([]byte(ref.OID))
		h.Write([]byte{0})
		h.Write([]byte(ref.Peeled))
		h.Write([]byte{0})
		h.Write([]byte(ref.Symref))
		h.Write([]byte{0})
	}
	return h.Sum(nil)
}
