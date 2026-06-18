package pekit

import "sync"

type DestinationRegistry struct {
	mu   sync.Mutex
	seen map[string]string
}

func NewDestinationRegistry() *DestinationRegistry {
	return &DestinationRegistry{seen: map[string]string{}}
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
