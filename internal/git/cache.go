package git

import (
	"sync"
	"time"
)

// probeTTL is how long what probe found about a repository is reused rather
// than asked for again.
//
// The change list is re-read every few seconds for as long as it is on screen,
// and every file opened from it asks the same questions over again: where the
// checkout starts, what is on HEAD, what this branch grew out of. Inside a
// sandbox each of those is a container round trip, which costs far more than
// the git command it carries. None of the answers moves except when the agent
// commits or changes branch, and a few seconds of staleness there means one
// refresh drawn against the previous base — which the next one corrects.
const probeTTL = 5 * time.Second

// probeSweepAt is the number of entries past which the expired ones are
// dropped. Sandboxes come and go, and nothing else would ever forget them.
const probeSweepAt = 256

// probeCache holds what probe found, keyed by sandbox and directory.
//
// It hangs off the Client as a pointer so that it survives InSandbox, which
// copies the Client — and is copied afresh for every request. A cache that
// went with those copies would never be read twice.
//
// The zero Client has none, and every method here tolerates that: caching is
// what keeps the sandbox round trips down, not something correctness rests on.
type probeCache struct {
	mu      sync.Mutex
	entries map[string]probeEntry
	// now is the clock, so a test can age an entry without waiting for one.
	now func() time.Time
}

type probeEntry struct {
	info repoInfo
	at   time.Time
}

func newProbeCache() *probeCache {
	return &probeCache{entries: make(map[string]probeEntry)}
}

func (p *probeCache) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// get returns what was cached for key, if it is still young enough to use.
func (p *probeCache) get(key string) (repoInfo, bool) {
	if p == nil {
		return repoInfo{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	e, ok := p.entries[key]
	if !ok || p.clock().Sub(e.at) >= probeTTL {
		return repoInfo{}, false
	}
	return e.info, true
}

func (p *probeCache) put(key string, info repoInfo) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	now := p.clock()
	if len(p.entries) >= probeSweepAt {
		for k, e := range p.entries {
			if now.Sub(e.at) >= probeTTL {
				delete(p.entries, k)
			}
		}
	}
	p.entries[key] = probeEntry{info: info, at: now}
}
