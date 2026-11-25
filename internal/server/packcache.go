package server

import (
	"crypto/sha256"

	"gitlab.com/banyan-vcs/git-cdn/internal/protocol"
)

// packCacheKey derives the verbatim response key for a request. Only
// cacheable fetches (done, no haves) participate: their request bytes
// repeat across clients, while incremental fetches embed client-specific
// haves that defeat reuse. Shallow and filtered shapes — git fetch
// --depth=1 or --filter fleets — carry their directives in the request
// bytes, so identical batches share one entry. The key binds the repository
// identity, the decoded request bytes as upload-pack sees them (wants,
// capabilities, deepen and filter directives) minus the agent line — the
// client's Git version never changes a response, and keying on it stored
// one entry per client version and emptied the cache on every fleet
// upgrade — and the live ref snapshot's parse-time fingerprint; a changed
// snapshot — new tips or new tags, which redirect include-tag output even
// for identical want sets — produces a new key and a fresh generation.
func (s *Server) packCacheKey(identity string, fetch protocol.Fetch, data []byte, snapshot protocol.Snapshot) []byte {
	if s.store.PackCacheBytes() <= 0 || !fetch.CacheableFetch() {
		return nil
	}
	h := sha256.New()
	h.Write([]byte(identity))
	h.Write([]byte{0})
	h.Write(protocol.FilterRequest(data, protocol.AgentLine))
	h.Write([]byte{0})
	fingerprint := snapshot.Fingerprint()
	h.Write(fingerprint[:])
	return h.Sum(nil)
}
