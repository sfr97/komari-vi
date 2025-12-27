package forward

import (
	"sync"
	"time"
)

type ForwardInstanceMeta struct {
	RuleID     uint
	NodeID     string
	InstanceID string
	Listen     string
	ListenPort int
	AllowTCP   bool
	AllowUDP   bool
	UpdatedAt  time.Time
}

type ForwardInstanceRegistry struct {
	mu sync.Mutex
	m  map[string]ForwardInstanceMeta
}

func NewForwardInstanceRegistry() *ForwardInstanceRegistry {
	return &ForwardInstanceRegistry{m: make(map[string]ForwardInstanceMeta)}
}

func (r *ForwardInstanceRegistry) Upsert(meta ForwardInstanceMeta) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[meta.InstanceID] = meta
}

func (r *ForwardInstanceRegistry) Delete(instanceID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, instanceID)
}

func (r *ForwardInstanceRegistry) Get(instanceID string) (ForwardInstanceMeta, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.m[instanceID]
	return v, ok
}

func (r *ForwardInstanceRegistry) Snapshot() []ForwardInstanceMeta {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ForwardInstanceMeta, 0, len(r.m))
	for _, v := range r.m {
		out = append(out, v)
	}
	return out
}
