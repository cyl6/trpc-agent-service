package tenant

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/cyl6/trpc-agent-service/trpcservice/config"
)

var (
	ErrTenantNotFound   = errors.New("tenant not found")
	ErrBindingNotFound  = errors.New("channel binding not found")
	ErrNoRollback       = errors.New("no previous tenant revision")
	ErrBindingConflict  = errors.New("channel binding conflict")
	ErrVersionImmutable = errors.New("tenant version is immutable")
)

type Binding struct {
	Tenant  config.TenantConfig
	Channel config.ChannelConfig
}

type snapshot struct {
	tenants  map[string]config.TenantConfig
	bindings map[string]Binding
}

// Registry publishes immutable configuration snapshots and retains one prior
// revision per tenant for an immediate operational rollback.
type Registry struct {
	mu        sync.RWMutex
	current   snapshot
	history   map[string][]config.TenantConfig
	revisions map[string]map[string][sha256.Size]byte
}

func NewRegistry(cfg *config.Config) (*Registry, error) {
	r := &Registry{
		history:   make(map[string][]config.TenantConfig),
		revisions: make(map[string]map[string][sha256.Size]byte),
	}
	if err := r.Apply(cfg); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Registry) Apply(cfg *config.Config) error {
	if cfg == nil {
		return errors.New("tenant registry: nil config")
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	next := snapshot{tenants: make(map[string]config.TenantConfig), bindings: make(map[string]Binding)}
	for _, tenant := range cfg.Tenants {
		next.tenants[tenant.TenantID] = tenant
		if !tenant.Enabled {
			continue
		}
		for _, ch := range tenant.Channels {
			if ch.Enabled {
				next.bindings[bindingKey(ch.Type, ch.BindingID)] = Binding{Tenant: tenant, Channel: ch}
			}
		}
	}

	digests := make(map[string][sha256.Size]byte, len(next.tenants))
	for id, tenantConfig := range next.tenants {
		digest, err := revisionDigest(tenantConfig)
		if err != nil {
			return fmt.Errorf("tenant %s revision digest: %w", id, err)
		}
		digests[id] = digest
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for id, newTenant := range next.tenants {
		if known, exists := r.revisions[id][newTenant.Version]; exists && known != digests[id] {
			return fmt.Errorf("%w: tenant %s changed without incrementing version %s", ErrVersionImmutable, id, newTenant.Version)
		}
	}
	for id, old := range r.current.tenants {
		newTenant, exists := next.tenants[id]
		if !exists || newTenant.Version != old.Version {
			r.history[id] = appendBounded(r.history[id], old, 10)
		}
	}
	for id, tenantConfig := range next.tenants {
		if r.revisions[id] == nil {
			r.revisions[id] = make(map[string][sha256.Size]byte)
		}
		r.revisions[id][tenantConfig.Version] = digests[id]
	}
	r.current = next
	return nil
}

func (r *Registry) Tenant(id string) (config.TenantConfig, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.current.tenants[id]
	if !ok || !t.Enabled {
		return config.TenantConfig{}, ErrTenantNotFound
	}
	return t, nil
}

func (r *Registry) ResolveBinding(channelType, bindingID string) (Binding, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	b, ok := r.current.bindings[bindingKey(channelType, bindingID)]
	if !ok {
		return Binding{}, ErrBindingNotFound
	}
	return b, nil
}

func (r *Registry) List() []config.TenantConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]config.TenantConfig, 0, len(r.current.tenants))
	for _, t := range r.current.tenants {
		result = append(result, t)
	}
	return result
}

func (r *Registry) Rollback(tenantID string) (config.TenantConfig, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	versions := r.history[tenantID]
	if len(versions) == 0 {
		return config.TenantConfig{}, ErrNoRollback
	}
	previous := versions[len(versions)-1]
	candidateTenants := make(map[string]config.TenantConfig, len(r.current.tenants)+1)
	for id, tenantConfig := range r.current.tenants {
		candidateTenants[id] = tenantConfig
	}
	candidateTenants[tenantID] = previous
	candidateBindings, err := buildBindings(candidateTenants)
	if err != nil {
		// Keep both the active snapshot and rollback history untouched. A stale
		// revision may contain a binding that another tenant legitimately owns
		// now; publishing it would route verified traffic across tenants.
		return config.TenantConfig{}, err
	}

	r.history[tenantID] = versions[:len(versions)-1]
	current, exists := r.current.tenants[tenantID]
	if exists {
		r.history[tenantID] = appendBounded(r.history[tenantID], current, 10)
	}
	r.current = snapshot{tenants: candidateTenants, bindings: candidateBindings}
	return previous, nil
}

func buildBindings(tenants map[string]config.TenantConfig) (map[string]Binding, error) {
	bindings := make(map[string]Binding)
	for _, tenantConfig := range tenants {
		if !tenantConfig.Enabled {
			continue
		}
		for _, ch := range tenantConfig.Channels {
			if !ch.Enabled {
				continue
			}
			key := bindingKey(ch.Type, ch.BindingID)
			if existing, ok := bindings[key]; ok && existing.Tenant.TenantID != tenantConfig.TenantID {
				return nil, fmt.Errorf("%w: %s is owned by tenants %s and %s", ErrBindingConflict, key, existing.Tenant.TenantID, tenantConfig.TenantID)
			}
			bindings[key] = Binding{Tenant: tenantConfig, Channel: ch}
		}
	}
	return bindings, nil
}

func bindingKey(channelType, bindingID string) string {
	return fmt.Sprintf("%s/%s", channelType, bindingID)
}

func appendBounded(items []config.TenantConfig, item config.TenantConfig, limit int) []config.TenantConfig {
	items = append(items, item)
	if len(items) > limit {
		items = append([]config.TenantConfig(nil), items[len(items)-limit:]...)
	}
	return items
}

func revisionDigest(tenantConfig config.TenantConfig) ([sha256.Size]byte, error) {
	encoded, err := json.Marshal(tenantConfig)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(encoded), nil
}
