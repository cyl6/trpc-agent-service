package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/cyl6/trpc-agent-service/trpcservice/config"
)

func TestMemoryServiceExposesExactlyTenantAllowedTools(t *testing.T) {
	tenantConfig := config.TenantConfig{
		TenantID: "tenant-a",
		Tools: config.ToolPolicy{
			Allow: []string{"memory_search", "memory_clear"},
			Deny:  []string{"memory_add"},
		},
		Data: config.DataConfig{Memory: config.BackendConfig{Type: "inmemory"}},
	}
	service, err := buildMemoryService(tenantConfig, "app")
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	got := map[string]bool{}
	for _, memoryTool := range service.Tools() {
		got[memoryTool.Declaration().Name] = true
	}
	if len(got) != 2 || !got["memory_search"] || !got["memory_clear"] {
		t.Fatalf("memory tool surface = %v", got)
	}
}

func TestManagerReusesInterleavedTenantRevisions(t *testing.T) {
	manager := NewManager()
	defer manager.Close()
	base := config.TenantConfig{
		TenantID: "tenant-a", Version: "v1", Enabled: true,
		App:   config.AppConfig{Name: "assistant", AgentName: "agent"},
		Model: config.ModelConfig{Provider: "mock", Name: "mock"},
		Data: config.DataConfig{
			Session: config.BackendConfig{Type: "inmemory"},
			Memory:  config.BackendConfig{Type: "disabled"},
		},
	}
	v1, releaseV1, err := manager.Acquire(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	releaseV1()
	v2Config := base
	v2Config.Version = "v2"
	v2, releaseV2, err := manager.Acquire(context.Background(), v2Config)
	if err != nil {
		t.Fatal(err)
	}
	releaseV2()
	if v1 == v2 {
		t.Fatal("different revisions unexpectedly shared a runtime")
	}
	v1Again, releaseAgain, err := manager.Acquire(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	releaseAgain()
	if v1Again != v1 {
		t.Fatal("interleaved historical revision was rebuilt instead of reused")
	}
}

func TestRedisBackendConstructionErrorDoesNotLeakDSN(t *testing.T) {
	const (
		envName    = "TEST_PLATFORM_REDIS_DSN"
		credential = "canary-password"
	)
	t.Setenv(envName, "redis://user:"+credential+"@localhost/%zz")
	tenantConfig := config.TenantConfig{
		TenantID: "tenant-a",
		Data: config.DataConfig{
			Session: config.BackendConfig{Type: "redis", DSNEnv: envName},
			Memory:  config.BackendConfig{Type: "redis", DSNEnv: envName},
		},
	}
	if _, err := buildSessionService(tenantConfig, "app"); err == nil || strings.Contains(err.Error(), credential) {
		t.Fatalf("session backend error = %v", err)
	}
	if _, err := buildMemoryService(tenantConfig, "app"); err == nil || strings.Contains(err.Error(), credential) {
		t.Fatalf("memory backend error = %v", err)
	}
}
