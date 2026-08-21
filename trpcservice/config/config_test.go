package config

import (
	"strings"
	"testing"
)

const validYAML = `
server: {address: ":0"}
coordination: {backend: inmemory}
telemetry: {service_name: test}
tenants:
  - tenant_id: tenant-a
    version: v1
    enabled: true
    app: {name: assistant, agent_name: chat-agent}
    model: {provider: mock, name: mock}
    tools: {allow: [calculator], deny: []}
    channels:
      - {type: telegram, binding_id: telegram-main, enabled: true, token_env: TG_TOKEN, signing_secret_env: TG_SECRET}
      - {type: slack, binding_id: slack-main, enabled: true, token_env: SLACK_TOKEN, signing_secret_env: SLACK_SECRET, workspace_id: T_TEST, application_id: A_TEST}
    data:
      session: {type: inmemory, namespace: ta}
      memory: {type: inmemory}
      summary: {type: inmemory}
      artifact: {type: inmemory}
      knowledge: {type: disabled}
      audit_log: {type: stdout}
    audit: {enabled: true, sink: stdout}
    budget: {requests_per_minute: 10, max_input_chars: 2000}
`

func TestDecodeDefaultsAndSecretReferences(t *testing.T) {
	cfg, err := Decode(strings.NewReader(validYAML))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.QueueSize == 0 || cfg.Coordination.DedupTTL == 0 {
		t.Fatal("defaults were not applied")
	}
	names := strings.Join(cfg.SecretEnvNames(), ",")
	for _, want := range []string{"TG_TOKEN", "TG_SECRET", "SLACK_TOKEN", "SLACK_SECRET"} {
		if !strings.Contains(names, want) {
			t.Fatalf("missing secret reference %s in %s", want, names)
		}
	}
}

func TestDecodeRejectsUnknownField(t *testing.T) {
	_, err := Decode(strings.NewReader(validYAML + "unknown: true\n"))
	if err == nil || !strings.Contains(err.Error(), "field unknown") {
		t.Fatalf("expected strict YAML error, got %v", err)
	}
}

func TestValidateRejectsCrossTenantBindingCollision(t *testing.T) {
	cfg, err := Decode(strings.NewReader(validYAML))
	if err != nil {
		t.Fatal(err)
	}
	duplicate := cfg.Tenants[0]
	duplicate.TenantID = "tenant-b"
	cfg.Tenants = append(cfg.Tenants, duplicate)
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "shared by tenants") {
		t.Fatalf("expected binding collision, got %v", err)
	}
}

func TestValidateRejectsDeclaredButUncompiledMemoryBackend(t *testing.T) {
	cfg, err := Decode(strings.NewReader(validYAML))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Tenants[0].Data.Memory = BackendConfig{Type: "sql", DSNEnv: "SQL_DSN"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "runnable memory backend") {
		t.Fatalf("expected fail-closed memory backend validation, got %v", err)
	}
}

func TestValidateRejectsLiteralSecretInEnvironmentReference(t *testing.T) {
	for _, literal := range []string{"xoxb-canary-secret", "redis://user:canary-password@redis:6379/0"} {
		cfg, err := Decode(strings.NewReader(validYAML))
		if err != nil {
			t.Fatal(err)
		}
		cfg.Tenants[0].Channels[0].TokenEnv = literal
		err = cfg.Validate()
		if err == nil {
			t.Fatalf("literal secret was accepted as an env reference")
		}
		if strings.Contains(err.Error(), "canary") {
			t.Fatalf("validation error echoed literal secret: %v", err)
		}
	}
}

func TestSecretRejectsLiteralWithoutEchoingIt(t *testing.T) {
	const literal = "redis://user:canary-password@redis:6379/0"
	_, err := Secret(literal)
	if err == nil || strings.Contains(err.Error(), "canary-password") {
		t.Fatalf("unsafe secret reference error = %v", err)
	}
}
