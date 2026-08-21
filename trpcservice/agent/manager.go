package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/cyl6/trpc-agent-service/trpcservice/config"
	"github.com/cyl6/trpc-agent-service/trpcservice/domain"
	platformtool "github.com/cyl6/trpc-agent-service/trpcservice/tool"

	"golang.org/x/sync/singleflight"
	"trpc.group/trpc-go/trpc-agent-go/agent"
	"trpc.group/trpc-go/trpc-agent-go/agent/llmagent"
	artifactinmemory "trpc.group/trpc-go/trpc-agent-go/artifact/inmemory"
	"trpc.group/trpc-go/trpc-agent-go/memory"
	memoryinmemory "trpc.group/trpc-go/trpc-agent-go/memory/inmemory"
	memoryredis "trpc.group/trpc-go/trpc-agent-go/memory/redis"
	"trpc.group/trpc-go/trpc-agent-go/model"
	modelopenai "trpc.group/trpc-go/trpc-agent-go/model/openai"
	"trpc.group/trpc-go/trpc-agent-go/runner"
	"trpc.group/trpc-go/trpc-agent-go/session"
	sessioninmemory "trpc.group/trpc-go/trpc-agent-go/session/inmemory"
	sessionredis "trpc.group/trpc-go/trpc-agent-go/session/redis"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

type Runtime struct {
	Runner       runner.Runner
	AppNamespace string
	Session      session.Service
	Memory       memory.Service
}

func (r *Runtime) close() error {
	if r == nil {
		return nil
	}
	var err error
	if r.Runner != nil {
		err = errors.Join(err, r.Runner.Close())
	}
	if r.Memory != nil {
		err = errors.Join(err, r.Memory.Close())
	}
	if r.Session != nil {
		err = errors.Join(err, r.Session.Close())
	}
	return err
}

type handle struct {
	runtime *Runtime
	refs    int
}

// Manager caches immutable Runtimes by tenant, revision, and canonical config
// digest. A singleflight build prevents duplicate connection pools without
// holding the global cache mutex while a backend is contacted. Revisions are
// retained for the process lifetime so delayed tasks can safely reuse the
// exact Runtime they were enqueued with.
type Manager struct {
	mu      sync.Mutex
	handles map[string]*handle
	builds  singleflight.Group
	closed  bool
}

func NewManager() *Manager {
	return &Manager{handles: make(map[string]*handle)}
}

func (m *Manager) Acquire(ctx context.Context, tenant config.TenantConfig) (*Runtime, func(), error) {
	key, err := runtimeCacheKey(tenant)
	if err != nil {
		return nil, nil, err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, nil, errors.New("runtime manager is closed")
	}
	if existing := m.handles[key]; existing != nil {
		existing.refs++
		m.mu.Unlock()
		return existing.runtime, m.releaseFunc(existing), nil
	}
	m.mu.Unlock()

	value, err, _ := m.builds.Do(key, func() (any, error) {
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return nil, errors.New("runtime manager is closed")
		}
		if existing := m.handles[key]; existing != nil {
			m.mu.Unlock()
			return existing, nil
		}
		m.mu.Unlock()

		built, buildErr := buildRuntime(ctx, tenant)
		if buildErr != nil {
			return nil, buildErr
		}
		next := &handle{runtime: built}
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			_ = built.close()
			return nil, errors.New("runtime manager is closed")
		}
		if existing := m.handles[key]; existing != nil {
			m.mu.Unlock()
			_ = built.close()
			return existing, nil
		}
		m.handles[key] = next
		m.mu.Unlock()
		return next, nil
	})
	if err != nil {
		return nil, nil, err
	}
	h := value.(*handle)
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, nil, errors.New("runtime manager is closed")
	}
	h.refs++
	m.mu.Unlock()
	return h.runtime, m.releaseFunc(h), nil
}

func (m *Manager) releaseFunc(h *handle) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			if h.refs > 0 {
				h.refs--
			}
			m.mu.Unlock()
		})
	}
}

func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	all := make([]*Runtime, 0, len(m.handles))
	for _, h := range m.handles {
		all = append(all, h.runtime)
	}
	m.handles = nil
	m.mu.Unlock()
	var result error
	for _, rt := range all {
		result = errors.Join(result, rt.close())
	}
	return result
}

func runtimeCacheKey(tenant config.TenantConfig) (string, error) {
	encoded, err := json.Marshal(tenant)
	if err != nil {
		return "", fmt.Errorf("encode tenant runtime revision: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return tenant.TenantID + "\x1f" + tenant.Version + "\x1f" + hex.EncodeToString(digest[:]), nil
}

func buildRuntime(_ context.Context, tenant config.TenantConfig) (*Runtime, error) {
	appNamespace := domain.AppNamespace(tenant.TenantID, tenant.App.Name)
	sessionService, err := buildSessionService(tenant, appNamespace)
	if err != nil {
		return nil, err
	}
	memoryService, err := buildMemoryService(tenant, appNamespace)
	if err != nil {
		_ = sessionService.Close()
		return nil, err
	}
	artifactService := artifactinmemory.NewService()
	tools := platformtool.BuiltInTools()
	if memoryService != nil {
		tools = append(tools, memoryService.Tools()...)
	}
	var rootAgent agent.Agent
	switch tenant.Model.Provider {
	case "mock":
		rootAgent = &mockAgent{name: tenant.App.AgentName, description: tenant.App.Description, tools: tools}
	case "openai":
		apiKey, secretErr := config.Secret(tenant.Model.APIKeyEnv)
		if secretErr != nil {
			if memoryService != nil {
				_ = memoryService.Close()
			}
			_ = sessionService.Close()
			return nil, secretErr
		}
		modelOptions := []modelopenai.Option{modelopenai.WithAPIKey(apiKey)}
		if tenant.Model.BaseURL != "" {
			modelOptions = append(modelOptions, modelopenai.WithBaseURL(tenant.Model.BaseURL))
		}
		if tenant.Model.Variant != "" {
			modelOptions = append(modelOptions, modelopenai.WithVariant(modelopenai.Variant(tenant.Model.Variant)))
		}
		llmModel := modelopenai.New(tenant.Model.Name, modelOptions...)
		maxTokens := tenant.Model.MaxTokens
		temperature := tenant.Model.Temperature
		agentOptions := []llmagent.Option{
			llmagent.WithModel(llmModel),
			llmagent.WithDescription(tenant.App.Description),
			llmagent.WithInstruction(tenant.App.Instruction),
			llmagent.WithTools(tools),
			llmagent.WithGenerationConfig(model.GenerationConfig{
				MaxTokens: &maxTokens, Temperature: &temperature, Stream: tenant.Model.Streaming,
			}),
			llmagent.WithAddSessionSummary(true),
			llmagent.WithMaxLLMCalls(8),
			llmagent.WithMaxToolIterations(6),
		}
		if memoryService != nil {
			agentOptions = append(agentOptions, llmagent.WithPreloadMemory(20))
		}
		rootAgent = llmagent.New(tenant.App.AgentName, agentOptions...)
	default:
		if memoryService != nil {
			_ = memoryService.Close()
		}
		_ = sessionService.Close()
		return nil, fmt.Errorf("unsupported model provider %q", tenant.Model.Provider)
	}
	runnerOptions := []runner.Option{runner.WithSessionService(sessionService), runner.WithArtifactService(artifactService)}
	if memoryService != nil {
		runnerOptions = append(runnerOptions, runner.WithMemoryService(memoryService))
	}
	r := runner.NewRunner(appNamespace, rootAgent, runnerOptions...)
	return &Runtime{Runner: r, AppNamespace: appNamespace, Session: sessionService, Memory: memoryService}, nil
}

func buildMemoryService(tenant config.TenantConfig, appNamespace string) (memory.Service, error) {
	memoryToolAllowed := func(name string) bool {
		for _, denied := range tenant.Tools.Deny {
			if denied == name {
				return false
			}
		}
		for _, allowed := range tenant.Tools.Allow {
			if allowed == name {
				return true
			}
		}
		return false
	}
	memoryToolNames := []string{
		memory.AddToolName, memory.UpdateToolName, memory.DeleteToolName,
		memory.ClearToolName, memory.SearchToolName, memory.LoadToolName,
	}
	switch tenant.Data.Memory.Type {
	case "disabled":
		return nil, nil
	case "inmemory":
		options := make([]memoryinmemory.ServiceOpt, 0, len(memoryToolNames))
		for _, name := range memoryToolNames {
			options = append(options, memoryinmemory.WithToolEnabled(name, memoryToolAllowed(name)))
		}
		return memoryinmemory.NewMemoryService(options...), nil
	case "redis":
		rawURL, err := config.Secret(tenant.Data.Memory.DSNEnv)
		if err != nil {
			return nil, err
		}
		prefix := tenant.Data.Memory.Namespace
		if prefix == "" {
			prefix = appNamespace
		}
		options := []memoryredis.ServiceOpt{
			memoryredis.WithRedisClientURL(rawURL),
			memoryredis.WithKeyPrefix(prefix),
		}
		for _, name := range memoryToolNames {
			options = append(options, memoryredis.WithToolEnabled(name, memoryToolAllowed(name)))
		}
		service, err := memoryredis.NewService(options...)
		if err != nil {
			return nil, errors.New("create redis memory service: invalid or unavailable backend")
		}
		return service, nil
	default:
		return nil, fmt.Errorf("unsupported runnable memory backend %q", tenant.Data.Memory.Type)
	}
}

func buildSessionService(tenant config.TenantConfig, appNamespace string) (session.Service, error) {
	switch tenant.Data.Session.Type {
	case "inmemory":
		return sessioninmemory.NewSessionService(), nil
	case "redis":
		rawURL, err := config.Secret(tenant.Data.Session.DSNEnv)
		if err != nil {
			return nil, err
		}
		prefix := tenant.Data.Session.Namespace
		if prefix == "" {
			prefix = appNamespace
		}
		service, err := sessionredis.NewService(
			sessionredis.WithRedisClientURL(rawURL),
			sessionredis.WithKeyPrefix(prefix),
			sessionredis.WithEnableTracing(true),
		)
		if err != nil {
			return nil, errors.New("create redis session service: invalid or unavailable backend")
		}
		return service, nil
	default:
		return nil, fmt.Errorf("unsupported runnable session backend %q", tenant.Data.Session.Type)
	}
}

// ToolNames is used by diagnostics and tests without exposing tool instances.
func ToolNames(tools []tool.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, candidate := range tools {
		if candidate != nil && candidate.Declaration() != nil {
			names = append(names, candidate.Declaration().Name)
		}
	}
	return names
}
