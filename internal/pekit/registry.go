package pekit

import "sync"

type DestinationRegistry struct {
	mu           sync.Mutex
	seen         map[string]string
	repositories map[string]repositoryReservation
	repoLocks    map[string]*sync.Mutex
}

func NewDestinationRegistry() *DestinationRegistry {
	return &DestinationRegistry{
		seen:         map[string]string{},
		repositories: map[string]repositoryReservation{},
		repoLocks:    map[string]*sync.Mutex{},
	}
}

type repositoryReservation struct {
	name       string
	signingKey string
}

func (r *DestinationRegistry) Reserve(path, owner string) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if prev, ok := r.seen[path]; ok {
		if prev == owner {
			return nil
		}
		return diagAt("publish_collision", path, "publish destination collision between %s and %s", prev, owner)
	}
	r.seen[path] = owner
	return nil
}

// ReservePeipkg permits many artifacts and workspace members to target one
// repository, but only when they agree on the identity and signing-key source
// that control its metadata.
func (r *DestinationRegistry) ReservePeipkg(path, name, signingKey string) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	next := repositoryReservation{name: name, signingKey: signingKey}
	if previous, ok := r.repositories[path]; ok {
		if previous != next {
			return diagAt("publish_collision", path,
				"peipkg repository target has conflicting name or signing_key settings")
		}
		return nil
	}
	r.repositories[path] = next
	r.repoLocks[path] = &sync.Mutex{}
	return nil
}

// WithPeipkg serializes state transitions for one repository. Workspace
// members publish concurrently, while peipkg indexes are whole signed
// snapshots; allowing two members to read the same revision and then both
// replace it would lose one member's entries.
func (r *DestinationRegistry) WithPeipkg(path string, fn func() error) error {
	if r == nil {
		return fn()
	}
	r.mu.Lock()
	lock := r.repoLocks[path]
	if lock == nil {
		lock = &sync.Mutex{}
		r.repoLocks[path] = lock
	}
	r.mu.Unlock()
	lock.Lock()
	defer lock.Unlock()
	return fn()
}
