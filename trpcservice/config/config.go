package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var (
	safeID       = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,62}$`)
	safeRevision = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	envReference = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

type Config struct {
	Server       ServerConfig       `yaml:"server" json:"server"`
	Coordination CoordinationConfig `yaml:"coordination" json:"coordination"`
	Telemetry    TelemetryConfig    `yaml:"telemetry" json:"telemetry"`
	Tenants      []TenantConfig     `yaml:"tenants" json:"tenants"`
}

type ServerConfig struct {
	Address       string `yaml:"address" json:"address"`
	QueueSize     int    `yaml:"queue_size" json:"queue_size"`
	WorkerCount   int    `yaml:"worker_count" json:"worker_count"`
	AdminTokenEnv string `yaml:"admin_token_env" json:"admin_token_env,omitempty"`
}

type CoordinationConfig struct {
	Backend     string        `yaml:"backend" json:"backend"`
	RedisURLEnv string        `yaml:"redis_url_env" json:"redis_url_env,omitempty"`
	KeyPrefix   string        `yaml:"key_prefix" json:"key_prefix"`
	LockTTL     time.Duration `yaml:"lock_ttl" json:"lock_ttl"`
	DedupTTL    time.Duration `yaml:"dedup_ttl" json:"dedup_ttl"`
}

type TelemetryConfig struct {
	ServiceName  string `yaml:"service_name" json:"service_name"`
	OTLPEndpoint string `yaml:"otlp_endpoint" json:"otlp_endpoint,omitempty"`
	OTLPProtocol string `yaml:"otlp_protocol" json:"otlp_protocol,omitempty"`
}

type TenantConfig struct {
	TenantID string          `yaml:"tenant_id" json:"tenant_id"`
	Version  string          `yaml:"version" json:"version"`
	Enabled  bool            `yaml:"enabled" json:"enabled"`
	App      AppConfig       `yaml:"app" json:"app"`
	Model    ModelConfig     `yaml:"model" json:"model"`
	Tools    ToolPolicy      `yaml:"tools" json:"tools"`
	Channels []ChannelConfig `yaml:"channels" json:"channels"`
	Data     DataConfig      `yaml:"data" json:"data"`
	Audit    AuditPolicy     `yaml:"audit" json:"audit"`
	Budget   BudgetPolicy    `yaml:"budget" json:"budget"`
}

type AppConfig struct {
	Name        string `yaml:"name" json:"name"`
	AgentName   string `yaml:"agent_name" json:"agent_name"`
	Description string `yaml:"description" json:"description"`
	Instruction string `yaml:"instruction" json:"instruction"`
}

type ModelConfig struct {
	Provider    string  `yaml:"provider" json:"provider"`
	Name        string  `yaml:"name" json:"name"`
	Variant     string  `yaml:"variant" json:"variant,omitempty"`
	BaseURL     string  `yaml:"base_url" json:"base_url,omitempty"`
	APIKeyEnv   string  `yaml:"api_key_env" json:"api_key_env,omitempty"`
	MaxTokens   int     `yaml:"max_tokens" json:"max_tokens"`
	Temperature float64 `yaml:"temperature" json:"temperature"`
	Streaming   bool    `yaml:"streaming" json:"streaming"`
	InputPrice  float64 `yaml:"input_price_per_million" json:"input_price_per_million,omitempty"`
	OutputPrice float64 `yaml:"output_price_per_million" json:"output_price_per_million,omitempty"`
}

type ToolPolicy struct {
	Allow          []string `yaml:"allow" json:"allow"`
	Deny           []string `yaml:"deny" json:"deny"`
	RequireConfirm []string `yaml:"require_confirmation" json:"require_confirmation"`
}

type ChannelConfig struct {
	Type             string   `yaml:"type" json:"type"`
	BindingID        string   `yaml:"binding_id" json:"binding_id"`
	Enabled          bool     `yaml:"enabled" json:"enabled"`
	TokenEnv         string   `yaml:"token_env" json:"token_env,omitempty"`
	SigningSecretEnv string   `yaml:"signing_secret_env" json:"signing_secret_env,omitempty"`
	EncryptionKeyEnv string   `yaml:"encryption_key_env" json:"encryption_key_env,omitempty"`
	APIBaseURL       string   `yaml:"api_base_url" json:"api_base_url,omitempty"`
	AllowedUsers     []string `yaml:"allowed_users" json:"allowed_users,omitempty"`
	MaxMessageLength int      `yaml:"max_message_length" json:"max_message_length,omitempty"`
	WorkspaceID      string   `yaml:"workspace_id" json:"workspace_id,omitempty"`
	ApplicationID    string   `yaml:"application_id" json:"application_id,omitempty"`
}

type DataConfig struct {
	Session   BackendConfig `yaml:"session" json:"session"`
	Memory    BackendConfig `yaml:"memory" json:"memory"`
	Summary   BackendConfig `yaml:"summary" json:"summary"`
	Artifact  BackendConfig `yaml:"artifact" json:"artifact"`
	Knowledge BackendConfig `yaml:"knowledge" json:"knowledge"`
	AuditLog  BackendConfig `yaml:"audit_log" json:"audit_log"`
}

type BackendConfig struct {
	Type      string `yaml:"type" json:"type"`
	DSNEnv    string `yaml:"dsn_env" json:"dsn_env,omitempty"`
	Namespace string `yaml:"namespace" json:"namespace,omitempty"`
	Bucket    string `yaml:"bucket" json:"bucket,omitempty"`
}

type AuditPolicy struct {
	Enabled        bool     `yaml:"enabled" json:"enabled"`
	Sink           string   `yaml:"sink" json:"sink"`
	Path           string   `yaml:"path" json:"path,omitempty"`
	RedactPatterns []string `yaml:"redact_patterns" json:"redact_patterns,omitempty"`
	LogContent     bool     `yaml:"log_content" json:"log_content"`
}

type BudgetPolicy struct {
	RequestsPerMinute int     `yaml:"requests_per_minute" json:"requests_per_minute"`
	MaxInputChars     int     `yaml:"max_input_chars" json:"max_input_chars"`
	MonthlyCostUSD    float64 `yaml:"monthly_cost_usd" json:"monthly_cost_usd"`
}

func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()
	return Decode(f)
}

func Decode(r io.Reader) (*Config, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	applyDefaults(&cfg)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func applyDefaults(c *Config) {
	if c.Server.Address == "" {
		c.Server.Address = ":8080"
	}
	if c.Server.QueueSize <= 0 {
		c.Server.QueueSize = 256
	}
	if c.Server.WorkerCount <= 0 {
		c.Server.WorkerCount = 4
	}
	if c.Coordination.Backend == "" {
		c.Coordination.Backend = "inmemory"
	}
	if c.Coordination.KeyPrefix == "" {
		c.Coordination.KeyPrefix = "trpc-agent"
	}
	if c.Coordination.LockTTL <= 0 {
		c.Coordination.LockTTL = 2 * time.Minute
	}
	if c.Coordination.DedupTTL <= 0 {
		c.Coordination.DedupTTL = 24 * time.Hour
	}
	if c.Telemetry.ServiceName == "" {
		c.Telemetry.ServiceName = "trpc-agent-service"
	}
	for i := range c.Tenants {
		t := &c.Tenants[i]
		if t.Version == "" {
			t.Version = "1"
		}
		if t.Model.MaxTokens <= 0 {
			t.Model.MaxTokens = 2048
		}
		if t.Budget.MaxInputChars <= 0 {
			t.Budget.MaxInputChars = 12000
		}
		if t.Budget.RequestsPerMinute <= 0 {
			t.Budget.RequestsPerMinute = 60
		}
		for j := range t.Channels {
			if t.Channels[j].MaxMessageLength <= 0 {
				switch t.Channels[j].Type {
				case "telegram":
					t.Channels[j].MaxMessageLength = 4096
				case "slack":
					t.Channels[j].MaxMessageLength = 40000
				case "wecom":
					// WeCom limits text content to 2048 UTF-8 bytes.
					t.Channels[j].MaxMessageLength = 2048
				default:
					t.Channels[j].MaxMessageLength = 4000
				}
			}
		}
	}
}

func (c *Config) Validate() error {
	if len(c.Tenants) == 0 {
		return errors.New("config: at least one tenant is required")
	}
	if c.Coordination.Backend != "inmemory" && c.Coordination.Backend != "redis" {
		return fmt.Errorf("config: unsupported coordination backend %q", c.Coordination.Backend)
	}
	if err := validateOptionalEnvReference("server.admin_token_env", c.Server.AdminTokenEnv); err != nil {
		return err
	}
	if c.Coordination.Backend == "redis" && c.Coordination.RedisURLEnv == "" {
		return errors.New("config: coordination.redis_url_env is required for redis")
	}
	if err := validateOptionalEnvReference("coordination.redis_url_env", c.Coordination.RedisURLEnv); err != nil {
		return err
	}
	if c.Coordination.LockTTL > 0 && c.Coordination.LockTTL < 3*time.Second {
		return errors.New("config: coordination.lock_ttl must be at least 3s")
	}
	if c.Telemetry.OTLPProtocol != "" && c.Telemetry.OTLPProtocol != "http" && c.Telemetry.OTLPProtocol != "grpc" {
		return fmt.Errorf("config: unsupported telemetry.otlp_protocol %q", c.Telemetry.OTLPProtocol)
	}
	tenantIDs := map[string]struct{}{}
	bindings := map[string]string{}
	for i := range c.Tenants {
		t := &c.Tenants[i]
		if !safeID.MatchString(t.TenantID) {
			return fmt.Errorf("config: invalid tenant_id %q", t.TenantID)
		}
		if !safeRevision.MatchString(t.Version) {
			return fmt.Errorf("config: tenant %s has invalid version", t.TenantID)
		}
		if _, exists := tenantIDs[t.TenantID]; exists {
			return fmt.Errorf("config: duplicate tenant_id %q", t.TenantID)
		}
		tenantIDs[t.TenantID] = struct{}{}
		if !safeID.MatchString(t.App.Name) || !safeID.MatchString(t.App.AgentName) {
			return fmt.Errorf("config: tenant %s has invalid app or agent name", t.TenantID)
		}
		if t.Model.Provider != "mock" && t.Model.Provider != "openai" {
			return fmt.Errorf("config: tenant %s has unsupported model provider %q", t.TenantID, t.Model.Provider)
		}
		if t.Model.Provider == "openai" && (t.Model.Name == "" || t.Model.APIKeyEnv == "") {
			return fmt.Errorf("config: tenant %s openai model requires name and api_key_env", t.TenantID)
		}
		if err := validateOptionalEnvReference("tenant model api_key_env", t.Model.APIKeyEnv); err != nil {
			return err
		}
		if err := validateTools(t.TenantID, t.Tools); err != nil {
			return err
		}
		if err := validateData(t); err != nil {
			return err
		}
		if t.Audit.Sink != "" && t.Audit.Sink != "stdout" && t.Audit.Sink != "file" && t.Audit.Sink != "disabled" {
			return fmt.Errorf("config: tenant %s has unsupported audit sink %q", t.TenantID, t.Audit.Sink)
		}
		if t.Audit.Sink == "file" && t.Audit.Path == "" {
			return fmt.Errorf("config: tenant %s file audit sink requires path", t.TenantID)
		}
		if t.Audit.LogContent {
			return fmt.Errorf("config: tenant %s raw audit content logging is not supported", t.TenantID)
		}
		for _, pattern := range t.Audit.RedactPatterns {
			if _, err := regexp.Compile(pattern); err != nil {
				return fmt.Errorf("config: tenant %s has invalid audit redact pattern: %w", t.TenantID, err)
			}
		}
		for _, ch := range t.Channels {
			if ch.Type != "telegram" && ch.Type != "slack" && ch.Type != "wecom" {
				return fmt.Errorf("config: tenant %s has unsupported channel %q", t.TenantID, ch.Type)
			}
			if !safeID.MatchString(ch.BindingID) {
				return fmt.Errorf("config: tenant %s has invalid binding_id %q", t.TenantID, ch.BindingID)
			}
			if ch.SigningSecretEnv == "" || ch.TokenEnv == "" {
				return fmt.Errorf("config: tenant %s channel %s requires token_env and signing_secret_env", t.TenantID, ch.BindingID)
			}
			if ch.Type == "slack" && (!safeRevision.MatchString(ch.WorkspaceID) || !safeRevision.MatchString(ch.ApplicationID)) {
				return fmt.Errorf("config: tenant %s Slack channel %s requires valid workspace_id and application_id", t.TenantID, ch.BindingID)
			}
			if ch.Type == "wecom" {
				// workspace_id is the corpid and application_id is the numeric
				// agentid; both bind the encrypted callback to one app.
				if !safeRevision.MatchString(ch.WorkspaceID) || !safeRevision.MatchString(ch.ApplicationID) {
					return fmt.Errorf("config: tenant %s WeCom channel %s requires valid workspace_id (corpid) and application_id (agentid)", t.TenantID, ch.BindingID)
				}
				if ch.EncryptionKeyEnv == "" {
					return fmt.Errorf("config: tenant %s WeCom channel %s requires encryption_key_env", t.TenantID, ch.BindingID)
				}
				if err := validateOptionalEnvReference("channel encryption_key_env", ch.EncryptionKeyEnv); err != nil {
					return err
				}
			}
			if err := validateOptionalEnvReference("channel token_env", ch.TokenEnv); err != nil {
				return err
			}
			if err := validateOptionalEnvReference("channel signing_secret_env", ch.SigningSecretEnv); err != nil {
				return err
			}
			key := ch.Type + "/" + ch.BindingID
			if owner, exists := bindings[key]; exists {
				return fmt.Errorf("config: binding %s is shared by tenants %s and %s", key, owner, t.TenantID)
			}
			bindings[key] = t.TenantID
		}
	}
	return nil
}

func validateTools(tenantID string, p ToolPolicy) error {
	denied := make(map[string]struct{}, len(p.Deny))
	for _, name := range p.Deny {
		denied[name] = struct{}{}
	}
	for _, name := range p.Allow {
		if _, exists := denied[name]; exists {
			return fmt.Errorf("config: tenant %s tool %q is both allowed and denied", tenantID, name)
		}
	}
	allowed := make(map[string]struct{}, len(p.Allow))
	for _, name := range p.Allow {
		allowed[name] = struct{}{}
	}
	for _, name := range p.RequireConfirm {
		if _, exists := allowed[name]; !exists {
			return fmt.Errorf("config: tenant %s confirmation tool %q must be allowed", tenantID, name)
		}
	}
	return nil
}

func validateData(t *TenantConfig) error {
	backends := []struct {
		name string
		cfg  BackendConfig
	}{
		{"session", t.Data.Session}, {"memory", t.Data.Memory},
		{"summary", t.Data.Summary}, {"artifact", t.Data.Artifact},
		{"knowledge", t.Data.Knowledge}, {"audit_log", t.Data.AuditLog},
	}
	allowed := map[string]bool{
		"disabled": true, "inmemory": true, "redis": true, "sql": true,
		"vector": true, "object": true, "external": true, "stdout": true,
	}
	for _, item := range backends {
		if item.cfg.Type == "" {
			return fmt.Errorf("config: tenant %s data.%s.type is required", t.TenantID, item.name)
		}
		if !allowed[item.cfg.Type] {
			return fmt.Errorf("config: tenant %s data.%s has unsupported type %q", t.TenantID, item.name, item.cfg.Type)
		}
		if (item.cfg.Type == "redis" || item.cfg.Type == "sql" || item.cfg.Type == "external") && item.cfg.DSNEnv == "" {
			return fmt.Errorf("config: tenant %s data.%s requires dsn_env", t.TenantID, item.name)
		}
		if err := validateOptionalEnvReference("data backend dsn_env", item.cfg.DSNEnv); err != nil {
			return err
		}
	}
	if t.Data.Session.Type != "inmemory" && t.Data.Session.Type != "redis" {
		return fmt.Errorf("config: tenant %s runnable session backend must be inmemory or redis", t.TenantID)
	}
	if t.Data.Memory.Type != "disabled" && t.Data.Memory.Type != "inmemory" && t.Data.Memory.Type != "redis" {
		return fmt.Errorf("config: tenant %s runnable memory backend must be disabled, inmemory or redis", t.TenantID)
	}
	return nil
}

// Secret resolves a secret by environment-variable name. Configuration stores
// references only, which keeps credentials out of YAML, logs and admin output.
func Secret(envName string) (string, error) {
	if !envReference.MatchString(envName) {
		return "", errors.New("invalid secret environment variable reference")
	}
	value, ok := os.LookupEnv(envName)
	if !ok || value == "" {
		return "", errors.New("secret environment variable is not set")
	}
	return value, nil
}

func validateOptionalEnvReference(field, value string) error {
	if value != "" && !envReference.MatchString(value) {
		return fmt.Errorf("config: %s must be an environment variable name", field)
	}
	return nil
}

// SecretEnvNames returns the configured secret references for redaction and
// startup diagnostics; it never returns secret values.
func (c *Config) SecretEnvNames() []string {
	set := map[string]struct{}{}
	add := func(s string) {
		if strings.TrimSpace(s) != "" {
			set[s] = struct{}{}
		}
	}
	add(c.Server.AdminTokenEnv)
	add(c.Coordination.RedisURLEnv)
	for _, t := range c.Tenants {
		for _, name := range t.SecretEnvNames() {
			add(name)
		}
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// SecretEnvNames returns only this tenant revision's secret references. It is
// used to keep audit redaction aligned with an in-flight immutable snapshot.
func (t TenantConfig) SecretEnvNames() []string {
	set := map[string]struct{}{}
	add := func(value string) {
		if strings.TrimSpace(value) != "" {
			set[value] = struct{}{}
		}
	}
	add(t.Model.APIKeyEnv)
	for _, ch := range t.Channels {
		add(ch.TokenEnv)
		add(ch.SigningSecretEnv)
		add(ch.EncryptionKeyEnv)
	}
	for _, backend := range []BackendConfig{t.Data.Session, t.Data.Memory, t.Data.Summary, t.Data.Artifact, t.Data.Knowledge, t.Data.AuditLog} {
		add(backend.DSNEnv)
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
