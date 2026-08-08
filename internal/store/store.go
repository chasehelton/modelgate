// Package store holds model rollout state.
//
// It is deliberately an INTERFACE with an in-memory implementation. The HTTP
// handlers only ever talk to the interface, so swapping in Postgres later is a
// new file in this package and zero changes anywhere else. That separation is
// the main architectural idea in this repo.
package store

import (
	"errors"
	"sync"

	"github.com/chasehelton/modelgate/internal/rollout"
)

var (
	ErrNotFound = errors.New("model not found")
	ErrBadInput = errors.New("invalid input")
)

// Store is the persistence boundary.
type Store interface {
	List() []rollout.Model
	Get(id string) (rollout.Model, error)
	Upsert(m rollout.Model) error
	SetPercent(id string, percent int) error
	SetDisabled(id string, disabled bool) error
	Ready() bool
}

// Memory is an in-process Store. Safe for concurrent use: every HTTP request is
// its own goroutine, so unguarded map access here would be a data race. Run the
// tests with -race to prove it.
type Memory struct {
	mu     sync.RWMutex
	models map[string]rollout.Model
	ready  bool
}

func NewMemory() *Memory {
	return &Memory{models: make(map[string]rollout.Model)}
}

// Seed loads starting state and marks the store ready. Until this is called
// Ready() is false, which keeps the K8s readiness probe failing and keeps
// traffic away from a pod that would serve wrong answers.
func (m *Memory) Seed(models []rollout.Model) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, mod := range models {
		m.models[mod.ID] = mod
	}
	m.ready = true
}

func (m *Memory) Ready() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.ready
}

func (m *Memory) List() []rollout.Model {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]rollout.Model, 0, len(m.models))
	for _, mod := range m.models {
		out = append(out, mod)
	}
	return out
}

func (m *Memory) Get(id string) (rollout.Model, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	mod, ok := m.models[id]
	if !ok {
		return rollout.Model{}, ErrNotFound
	}
	return mod, nil
}

func (m *Memory) Upsert(mod rollout.Model) error {
	if mod.ID == "" {
		return ErrBadInput
	}
	if mod.Percent < 0 || mod.Percent > 100 {
		return ErrBadInput
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.models[mod.ID] = mod
	return nil
}

func (m *Memory) SetPercent(id string, percent int) error {
	if percent < 0 || percent > 100 {
		return ErrBadInput
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	mod, ok := m.models[id]
	if !ok {
		return ErrNotFound
	}
	mod.Percent = percent
	m.models[id] = mod
	return nil
}

func (m *Memory) SetDisabled(id string, disabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	mod, ok := m.models[id]
	if !ok {
		return ErrNotFound
	}
	mod.Disabled = disabled
	m.models[id] = mod
	return nil
}

// TODO(exercise 3): add an audit log -- every percent change and kill-switch
// flip recorded with who/when/old/new. Real rollout systems are unusable in an
// incident without this ("who took it to 100% at 3am?").
//
// TODO(exercise 4): implement a Postgres Store. The handlers must not change.
// If you have to touch internal/httpapi to make it work, the interface is wrong.
