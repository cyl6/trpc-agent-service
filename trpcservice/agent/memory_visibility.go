package agent

import (
	"context"
	"errors"

	"github.com/cyl6/trpc-agent-service/trpcservice/memoryvisibility"
	"trpc.group/trpc-go/trpc-agent-go/memory"
	"trpc.group/trpc-go/trpc-agent-go/session"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

// visibleMemoryService adds an explicit, verifiable read-after-write fence to
// a framework memory backend. Without a minimum watermark the framework keeps
// its normal eventual-read behaviour; with one, a stale node returns a
// degraded/timeout error instead of claiming visibility it cannot prove.
type visibleMemoryService struct {
	inner  memory.Service
	store  memoryvisibility.Store
	tenant string
	app    string
}

func newVisibleMemoryService(inner memory.Service, store memoryvisibility.Store, tenant, app string) memory.Service {
	if inner == nil || store == nil {
		return inner
	}
	return &visibleMemoryService{inner: inner, store: store, tenant: tenant, app: app}
}

func (s *visibleMemoryService) scope(userKey memory.UserKey) memoryvisibility.Scope {
	return memoryvisibility.Scope{TenantID: s.tenant, AppName: s.app, PrincipalID: userKey.UserID}
}

func (s *visibleMemoryService) wait(ctx context.Context, userKey memory.UserKey) error {
	minimum, ok := memoryvisibility.Minimum(ctx)
	if !ok {
		return nil
	}
	_, err := s.store.WaitUntilVisible(ctx, s.scope(userKey), minimum)
	return err
}

func (s *visibleMemoryService) ReadMemories(ctx context.Context, userKey memory.UserKey, limit int) ([]*memory.Entry, error) {
	if err := s.wait(ctx, userKey); err != nil {
		return nil, err
	}
	return s.inner.ReadMemories(ctx, userKey, limit)
}

func (s *visibleMemoryService) SearchMemories(ctx context.Context, userKey memory.UserKey, query string, opts ...memory.SearchOption) ([]*memory.Entry, error) {
	if err := s.wait(ctx, userKey); err != nil {
		return nil, err
	}
	return s.inner.SearchMemories(ctx, userKey, query, opts...)
}

func (s *visibleMemoryService) AddMemory(ctx context.Context, userKey memory.UserKey, value string, topics []string, opts ...memory.AddOption) error {
	if err := s.inner.AddMemory(ctx, userKey, value, topics, opts...); err != nil {
		return err
	}
	_, err := s.store.WriteWatermark(ctx, s.scope(userKey))
	return err
}

func (s *visibleMemoryService) UpdateMemory(ctx context.Context, key memory.Key, value string, topics []string, opts ...memory.UpdateOption) error {
	if err := s.inner.UpdateMemory(ctx, key, value, topics, opts...); err != nil {
		return err
	}
	_, err := s.store.WriteWatermark(ctx, memoryvisibility.Scope{TenantID: s.tenant, AppName: s.app, PrincipalID: key.UserID})
	return err
}

func (s *visibleMemoryService) DeleteMemory(ctx context.Context, key memory.Key) error {
	if err := s.inner.DeleteMemory(ctx, key); err != nil {
		return err
	}
	_, err := s.store.WriteWatermark(ctx, memoryvisibility.Scope{TenantID: s.tenant, AppName: s.app, PrincipalID: key.UserID})
	return err
}

func (s *visibleMemoryService) ClearMemories(ctx context.Context, userKey memory.UserKey) error {
	if err := s.inner.ClearMemories(ctx, userKey); err != nil {
		return err
	}
	_, err := s.store.WriteWatermark(ctx, s.scope(userKey))
	return err
}

func (s *visibleMemoryService) Tools() []tool.Tool { return s.inner.Tools() }

func (s *visibleMemoryService) EnqueueAutoMemoryJob(ctx context.Context, sess *session.Session) error {
	if err := s.inner.EnqueueAutoMemoryJob(ctx, sess); err != nil {
		return err
	}
	if sess == nil {
		return errors.New("memory visibility: session is nil")
	}
	_, err := s.store.WriteWatermark(ctx, memoryvisibility.Scope{
		TenantID: s.tenant, AppName: s.app, PrincipalID: sess.UserID,
	})
	return err
}

func (s *visibleMemoryService) Close() error { return s.inner.Close() }

var _ memory.Service = (*visibleMemoryService)(nil)
