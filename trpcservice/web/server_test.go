package web

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cyl6/trpc-agent-service/trpcservice/channels"
	"github.com/cyl6/trpc-agent-service/trpcservice/config"
	"github.com/cyl6/trpc-agent-service/trpcservice/metrics"
	"github.com/cyl6/trpc-agent-service/trpcservice/tenant"
	"github.com/cyl6/trpc-agent-service/trpcservice/worker"
)

type captureDispatcher struct {
	tasks chan worker.Task
}

const gatewayConfigYAML = `
server: {address: ":0", admin_token_env: ADMIN_TOKEN}
coordination: {backend: inmemory}
telemetry: {service_name: test}
tenants:
  - tenant_id: tenant-a
    version: v1
    enabled: true
    app: {name: assistant, agent_name: chat-agent}
    model: {provider: mock, name: mock}
    tools: {allow: [calculator]}
    channels:
      - {type: telegram, binding_id: tenant-a-hook, enabled: true, token_env: TG_TOKEN, signing_secret_env: TG_SECRET}
    data:
      session: {type: inmemory}
      memory: {type: inmemory}
      summary: {type: inmemory}
      artifact: {type: inmemory}
      knowledge: {type: disabled}
      audit_log: {type: stdout}
    audit: {enabled: true, sink: stdout}
    budget: {requests_per_minute: 10, max_input_chars: 1000}
`

func (d *captureDispatcher) Submit(_ context.Context, task worker.Task) error {
	d.tasks <- task
	return nil
}

func gatewayConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Decode(strings.NewReader(gatewayConfigYAML))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestReloadRejectsStaticConfigurationChanges(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "admin-test-token")
	cfg := gatewayConfig(t)
	registry, err := tenant.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	changed := strings.Replace(gatewayConfigYAML, `address: ":0"`, `address: ":9999"`, 1)
	if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	server := NewServer(registry, channels.NewRegistry(), nil, nil, metrics.NewMetrics(), "ADMIN_TOKEN", path, cfg)
	req := httptest.NewRequest(http.MethodPost, "/admin/v1/reload", nil)
	req.Header.Set("Authorization", "Bearer admin-test-token")
	res := httptest.NewRecorder()
	server.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusConflict {
		t.Fatalf("static reload status=%d body=%s", res.Code, res.Body.String())
	}
}

func TestTenantListOmitsPromptsSecretReferencesAndAllowedUsers(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "admin-test-token")
	cfg := gatewayConfig(t)
	cfg.Tenants[0].App.Instruction = "private-system-instruction"
	cfg.Tenants[0].Channels[0].AllowedUsers = []string{"private-external-user"}
	registry, err := tenant.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(registry, channels.NewRegistry(), nil, nil, metrics.NewMetrics(), "ADMIN_TOKEN", "", cfg)
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/tenants", nil)
	req.Header.Set("Authorization", "Bearer admin-test-token")
	res := httptest.NewRecorder()
	server.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"tenant_id":"tenant-a"`) {
		t.Fatalf("tenant list status=%d body=%s", res.Code, res.Body.String())
	}
	for _, forbidden := range []string{"private-system-instruction", "private-external-user", "TG_TOKEN", "TG_SECRET"} {
		if strings.Contains(res.Body.String(), forbidden) {
			t.Fatalf("tenant list leaked %q: %s", forbidden, res.Body.String())
		}
	}
}

func TestWebhookDerivesTenantFromVerifiedBinding(t *testing.T) {
	t.Setenv("TG_SECRET", "webhook-secret")
	t.Setenv("TG_TOKEN", "unused-token")
	cfg := gatewayConfig(t)
	registry, err := tenant.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dispatch := &captureDispatcher{tasks: make(chan worker.Task, 1)}
	channelRegistry := channels.NewRegistry(channels.NewTelegram(nil))
	server := NewServer(registry, channelRegistry, dispatch, nil, metrics.NewMetrics(), "ADMIN_TOKEN", "", nil)
	body := []byte(`{"tenant_id":"attacker-selected","update_id":42,"message":{"message_id":3,"date":1700000000,"text":"hello","from":{"id":7},"chat":{"id":9,"type":"private"}}}`)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/telegram/tenant-a-hook", bytes.NewReader(body))
	req.Header.Set("X-Telegram-Bot-Api-Secret-Token", "webhook-secret")
	res := httptest.NewRecorder()
	server.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
	}
	select {
	case task := <-dispatch.tasks:
		if task.Tenant.TenantID != "tenant-a" || task.Message.TenantID != "tenant-a" {
			t.Fatalf("untrusted tenant was accepted: %+v", task)
		}
		if task.Message.ExternalMessageID != "42" || !task.Deliver {
			t.Fatalf("unexpected normalized task: %+v", task)
		}
	case <-time.After(time.Second):
		t.Fatal("no task dispatched")
	}
}

func TestWebhookRejectsInvalidSignatureBeforeDispatch(t *testing.T) {
	t.Setenv("TG_SECRET", "webhook-secret")
	t.Setenv("TG_TOKEN", "unused-token")
	cfg := gatewayConfig(t)
	registry, _ := tenant.NewRegistry(cfg)
	dispatch := &captureDispatcher{tasks: make(chan worker.Task, 1)}
	server := NewServer(registry, channels.NewRegistry(channels.NewTelegram(nil)), dispatch, nil, metrics.NewMetrics(), "", "", nil)
	body := []byte(`{"update_id":42,"message":{"message_id":3,"text":"hello","from":{"id":7},"chat":{"id":9,"type":"private"}}}`)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/telegram/tenant-a-hook", bytes.NewReader(body))
	req.Header.Set("X-Telegram-Bot-Api-Secret-Token", "wrong")
	res := httptest.NewRecorder()
	server.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
	}
	select {
	case task := <-dispatch.tasks:
		t.Fatalf("invalid webhook was dispatched: %+v", task)
	default:
	}
}
