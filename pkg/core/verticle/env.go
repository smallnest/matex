package verticle

import "sync"

// rawService is guarded for hot reload: readers call DecodeService,
// the watcher swaps the section.
type envSection struct {
	mu  sync.RWMutex
	raw map[string]any
}

func (e *envSection) get() map[string]any {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.raw
}

func (e *envSection) set(m map[string]any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.raw = m
}
