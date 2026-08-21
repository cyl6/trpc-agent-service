package coordination

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

type ClaimState int

const (
	Claimed ClaimState = iota
	AlreadyProcessing
	AlreadyCompleted
)

var (
	ErrClaimOwnershipLost = errors.New("coordination: claim ownership lost")
	ErrLockOwnershipLost  = errors.New("coordination: lock ownership lost")
	ErrClaimInProgress    = errors.New("coordination: message claim is still processing")
)

// ClaimLease identifies the worker that owns a processing claim. The token is
// deliberately required by Complete and ReleaseClaim so a delayed worker
// cannot overwrite or delete a newer worker's claim after the first lease
// expires.
type ClaimLease struct {
	State ClaimState
	Token string
}

// LockLease exposes lease loss to the caller. Release is idempotent. A nil
// value is never returned on a successful Lock call.
type LockLease struct {
	Release func()
	Lost    <-chan error
}

type Coordinator interface {
	Claim(ctx context.Context, key string, ttl time.Duration) (ClaimLease, error)
	Complete(ctx context.Context, key, ownerToken string, ttl time.Duration) error
	ReleaseClaim(ctx context.Context, key, ownerToken string) error
	SaveResult(ctx context.Context, key, ownerToken string, value any, ttl time.Duration) error
	LoadResult(ctx context.Context, key string, value any) (bool, error)
	DeleteResult(ctx context.Context, key string) error
	Lock(ctx context.Context, key string, ttl time.Duration) (*LockLease, error)
	Close() error
}

type claim struct {
	state   ClaimState
	owner   string
	expires time.Time
}

type lockEntry struct {
	sem  chan struct{}
	refs int
}

type InMemory struct {
	mu      sync.Mutex
	claims  map[string]claim
	results map[string]storedResult
	locks   map[string]*lockEntry
}

type storedResult struct {
	value   []byte
	expires time.Time
}

func NewInMemory() *InMemory {
	return &InMemory{claims: make(map[string]claim), results: make(map[string]storedResult), locks: make(map[string]*lockEntry)}
}

func (m *InMemory) Claim(_ context.Context, key string, ttl time.Duration) (ClaimLease, error) {
	if key == "" {
		return ClaimLease{}, errors.New("coordination: empty claim key")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if existing, ok := m.claims[key]; ok && now.Before(existing.expires) {
		return ClaimLease{State: existing.state}, nil
	}
	owner := newOwnerToken()
	m.claims[key] = claim{state: AlreadyProcessing, owner: owner, expires: now.Add(ttl)}
	return ClaimLease{State: Claimed, Token: owner}, nil
}

func (m *InMemory) Complete(_ context.Context, key, ownerToken string, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.claims[key]
	if !ok || existing.state != AlreadyProcessing || existing.owner != ownerToken || time.Now().After(existing.expires) {
		return ErrClaimOwnershipLost
	}
	m.claims[key] = claim{state: AlreadyCompleted, expires: time.Now().Add(ttl)}
	return nil
}

func (m *InMemory) ReleaseClaim(_ context.Context, key, ownerToken string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.claims[key]; ok && existing.state == AlreadyProcessing && existing.owner == ownerToken {
		delete(m.claims, key)
	}
	return nil
}

func (m *InMemory) SaveResult(_ context.Context, key, ownerToken string, value any, ttl time.Duration) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode pending result: %w", err)
	}
	m.mu.Lock()
	claimValue, ok := m.claims[key]
	if !ok || claimValue.state != AlreadyProcessing || claimValue.owner != ownerToken || time.Now().After(claimValue.expires) {
		m.mu.Unlock()
		return ErrClaimOwnershipLost
	}
	m.results[key] = storedResult{value: encoded, expires: time.Now().Add(ttl)}
	m.mu.Unlock()
	return nil
}

func (m *InMemory) LoadResult(_ context.Context, key string, value any) (bool, error) {
	m.mu.Lock()
	item, ok := m.results[key]
	if ok && time.Now().After(item.expires) {
		delete(m.results, key)
		ok = false
	}
	m.mu.Unlock()
	if !ok {
		return false, nil
	}
	if err := json.Unmarshal(item.value, value); err != nil {
		return false, fmt.Errorf("decode pending result: %w", err)
	}
	return true, nil
}

func (m *InMemory) DeleteResult(_ context.Context, key string) error {
	m.mu.Lock()
	delete(m.results, key)
	m.mu.Unlock()
	return nil
}

func (m *InMemory) Lock(ctx context.Context, key string, _ time.Duration) (*LockLease, error) {
	if key == "" {
		return nil, errors.New("coordination: empty lock key")
	}
	m.mu.Lock()
	entry := m.locks[key]
	if entry == nil {
		entry = &lockEntry{sem: make(chan struct{}, 1)}
		entry.sem <- struct{}{}
		m.locks[key] = entry
	}
	entry.refs++
	m.mu.Unlock()

	select {
	case <-ctx.Done():
		m.releaseRef(key, entry)
		return nil, fmt.Errorf("acquire session lock: %w", ctx.Err())
	case <-entry.sem:
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			entry.sem <- struct{}{}
			m.releaseRef(key, entry)
		})
	}
	return &LockLease{Release: release, Lost: make(chan error)}, nil
}

func (m *InMemory) releaseRef(key string, entry *lockEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry.refs--
	if entry.refs == 0 && m.locks[key] == entry {
		delete(m.locks, key)
	}
}

func (*InMemory) Close() error { return nil }

func newOwnerToken() string { return uuid.NewString() }
