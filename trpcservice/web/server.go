package web

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/cyl6/trpc-agent-service/trpcservice"
	"github.com/cyl6/trpc-agent-service/trpcservice/channels"
	"github.com/cyl6/trpc-agent-service/trpcservice/config"
	"github.com/cyl6/trpc-agent-service/trpcservice/domain"
	"github.com/cyl6/trpc-agent-service/trpcservice/metrics"
	"github.com/cyl6/trpc-agent-service/trpcservice/queue"
	"github.com/cyl6/trpc-agent-service/trpcservice/tenant"
	"github.com/cyl6/trpc-agent-service/trpcservice/worker"
)

//go:embed index.html
var indexHTML []byte

const maxWebhookBody = 1 << 20

var tracer = otel.Tracer("trpc-agent-service/web")

type DirectProcessor interface {
	Process(context.Context, worker.Task) (worker.Result, error)
}

type Server struct {
	tenants       *tenant.Registry
	channels      *channels.Registry
	dispatcher    queue.Dispatcher
	direct        DirectProcessor
	metrics       *metrics.Metrics
	adminTokenEnv string
	configPath    string
	staticConfig  *config.Config
	mux           *http.ServeMux
}

func NewServer(
	tenants *tenant.Registry,
	channelRegistry *channels.Registry,
	dispatcher queue.Dispatcher,
	direct DirectProcessor,
	metricsExporter *metrics.Metrics,
	adminTokenEnv, configPath string,
	initialConfig *config.Config,
) *Server {
	if metricsExporter == nil {
		metricsExporter = metrics.NewMetrics()
	}
	s := &Server{
		tenants: tenants, channels: channelRegistry, dispatcher: dispatcher, direct: direct,
		metrics: metricsExporter, adminTokenEnv: adminTokenEnv, configPath: configPath,
		staticConfig: initialConfig,
		mux:          http.NewServeMux(),
	}
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	s.mux.HandleFunc("GET /{$}", s.index)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	s.mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	s.mux.Handle("GET /metrics", s.metrics)
	s.mux.HandleFunc("POST /webhooks/{channel}/{binding}", s.webhook)
	s.mux.HandleFunc("POST /v1/chat/{tenant}", s.requireAdmin(s.directChat))
	s.mux.HandleFunc("GET /admin/v1/tenants", s.requireAdmin(s.listTenants))
	s.mux.HandleFunc("POST /admin/v1/reload", s.requireAdmin(s.reload))
	s.mux.HandleFunc("POST /admin/v1/tenants/{tenant}/rollback", s.requireAdmin(s.rollback))
}

func (s *Server) webhook(w http.ResponseWriter, r *http.Request) {
	remoteCtx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
	startOptions := []oteltrace.SpanStartOption{oteltrace.WithNewRoot()}
	if remoteSpan := oteltrace.SpanContextFromContext(remoteCtx); remoteSpan.IsValid() {
		// The public callback header is untrusted. Link it for diagnostics, but
		// never let it choose our parent, sampling decision, or baggage.
		startOptions = append(startOptions, oteltrace.WithLinks(oteltrace.Link{SpanContext: remoteSpan}))
	}
	ctx, span := tracer.Start(r.Context(), "im.callback", startOptions...)
	defer span.End()
	channelType := r.PathValue("channel")
	bindingID := r.PathValue("binding")
	binding, err := s.tenants.ResolveBinding(channelType, bindingID)
	if err != nil {
		span.SetStatus(codes.Error, "binding not found")
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	span.SetAttributes(attribute.String("tenant.id", binding.Tenant.TenantID), attribute.String("channel", channelType))
	adapter, err := s.channels.Get(channelType)
	if err != nil {
		http.Error(w, "channel unavailable", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}
	verifyCtx, verifySpan := tracer.Start(ctx, "signature.verify")
	err = adapter.Verify(r.WithContext(verifyCtx), body, binding.Channel)
	verifySpan.End()
	if err != nil {
		span.SetStatus(codes.Error, "invalid_signature")
		s.metrics.Add("im_callbacks_total", "Inbound IM callback attempts.", 1, map[string]string{"tenant": binding.Tenant.TenantID, "channel": channelType, "result": "invalid_signature"})
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	parsed, err := adapter.Parse(body, binding.Channel)
	if errors.Is(err, channels.ErrUnsupportedEvent) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}
	if err != nil {
		span.SetStatus(codes.Error, "invalid_event")
		http.Error(w, "bad event", http.StatusBadRequest)
		return
	}
	if parsed.Challenge != "" {
		writeJSON(w, http.StatusOK, map[string]string{"challenge": parsed.Challenge})
		return
	}
	for _, msg := range parsed.Messages {
		msg.TenantID = binding.Tenant.TenantID
		msg.BindingID = binding.Channel.BindingID
		msg.Channel = binding.Channel.Type
		carrier := propagation.MapCarrier{}
		otel.GetTextMapPropagator().Inject(ctx, carrier)
		task := worker.Task{
			Tenant: binding.Tenant, Binding: binding.Channel, Message: msg,
			Deliver: true, TraceCarrier: map[string]string(carrier),
		}
		if err := s.dispatcher.Submit(ctx, task); err != nil {
			span.SetStatus(codes.Error, "queue_unavailable")
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
	}
	s.metrics.Add("im_callbacks_total", "Inbound IM callback attempts.", 1, map[string]string{"tenant": binding.Tenant.TenantID, "channel": channelType, "result": "accepted"})
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

type directRequest struct {
	MessageID      string       `json:"message_id"`
	UserID         string       `json:"user_id"`
	ConversationID string       `json:"conversation_id"`
	ThreadID       string       `json:"thread_id,omitempty"`
	Scope          domain.Scope `json:"scope"`
	Text           string       `json:"text"`
}

func (s *Server) directChat(w http.ResponseWriter, r *http.Request) {
	if s.direct == nil {
		http.Error(w, "direct processor unavailable", http.StatusServiceUnavailable)
		return
	}
	tenantConfig, err := s.tenants.Tenant(r.PathValue("tenant"))
	if err != nil {
		http.Error(w, "tenant not found", http.StatusNotFound)
		return
	}
	var request directRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxWebhookBody)).Decode(&request); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if request.UserID == "" || strings.TrimSpace(request.Text) == "" {
		http.Error(w, "user_id and text are required", http.StatusBadRequest)
		return
	}
	if request.MessageID == "" {
		request.MessageID = uuid.NewString()
	}
	if request.Scope == "" {
		request.Scope = domain.ScopeDirect
	}
	if request.Scope != domain.ScopeDirect && request.Scope != domain.ScopeGroup {
		http.Error(w, "scope must be direct or group", http.StatusBadRequest)
		return
	}
	if request.ConversationID == "" {
		request.ConversationID = request.UserID
	}
	binding := config.ChannelConfig{Type: "api", BindingID: "admin-api", Enabled: true, MaxMessageLength: 40000}
	result, err := s.direct.Process(r.Context(), worker.Task{
		Tenant: tenantConfig, Binding: binding, Deliver: false,
		Message: domain.InboundMessage{
			TenantID: tenantConfig.TenantID, BindingID: binding.BindingID, Channel: binding.Type,
			ExternalMessageID: request.MessageID, ExternalUserID: request.UserID,
			ConversationID: request.ConversationID, ThreadID: request.ThreadID,
			Scope: request.Scope, Text: request.Text, ReceivedAt: time.Now().UTC(),
		},
	})
	if err != nil {
		writeJSON(w, statusFor(err), map[string]string{"error": "request rejected or processing failed"})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) listTenants(w http.ResponseWriter, _ *http.Request) {
	tenants := s.tenants.List()
	redacted := make([]map[string]any, 0, len(tenants))
	for _, tenantConfig := range tenants {
		channels := make([]map[string]any, 0, len(tenantConfig.Channels))
		for _, binding := range tenantConfig.Channels {
			channels = append(channels, map[string]any{
				"type": binding.Type, "binding_id": binding.BindingID, "enabled": binding.Enabled,
			})
		}
		redacted = append(redacted, map[string]any{
			"tenant_id": tenantConfig.TenantID, "version": tenantConfig.Version, "enabled": tenantConfig.Enabled,
			"app": map[string]any{"name": tenantConfig.App.Name, "agent_name": tenantConfig.App.AgentName},
			"model": map[string]any{
				"provider": tenantConfig.Model.Provider, "name": tenantConfig.Model.Name,
				"variant": tenantConfig.Model.Variant, "streaming": tenantConfig.Model.Streaming,
			},
			"channels": channels,
			"data": map[string]string{
				"session": tenantConfig.Data.Session.Type, "memory": tenantConfig.Data.Memory.Type,
				"summary": tenantConfig.Data.Summary.Type, "artifact": tenantConfig.Data.Artifact.Type,
				"knowledge": tenantConfig.Data.Knowledge.Type, "audit_log": tenantConfig.Data.AuditLog.Type,
			},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tenants": redacted})
}

func (s *Server) reload(w http.ResponseWriter, _ *http.Request) {
	if s.configPath == "" {
		http.Error(w, "config reload is disabled", http.StatusNotImplemented)
		return
	}
	cfg, err := config.Load(s.configPath)
	if err != nil {
		http.Error(w, "invalid configuration", http.StatusBadRequest)
		return
	}
	if s.staticConfig != nil && (cfg.Server != s.staticConfig.Server || cfg.Coordination != s.staticConfig.Coordination || cfg.Telemetry != s.staticConfig.Telemetry) {
		http.Error(w, "reload only accepts tenant revisions; restart to change server, coordination, or telemetry", http.StatusConflict)
		return
	}
	if err := s.tenants.Apply(cfg); err != nil {
		http.Error(w, "configuration rejected", http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "reloaded"})
}

func (s *Server) rollback(w http.ResponseWriter, r *http.Request) {
	revision, err := s.tenants.Rollback(r.PathValue("tenant"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "rolled_back", "version": revision.Version})
}

func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.adminTokenEnv == "" {
			http.Error(w, "admin API disabled", http.StatusNotFound)
			return
		}
		secret, err := config.Secret(s.adminTokenEnv)
		if err != nil {
			http.Error(w, "admin API unavailable", http.StatusServiceUnavailable)
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if len(got) != len(secret) || subtle.ConstantTimeCompare([]byte(got), []byte(secret)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) index(w http.ResponseWriter, _ *http.Request) {
	page := strings.ReplaceAll(string(indexHTML), "__VERSION__", trpcservice.Version)
	page = strings.ReplaceAll(page, "__COMMIT__", trpcservice.GitCommit)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, page)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func statusFor(err error) int {
	if err == nil {
		return http.StatusOK
	}
	return http.StatusUnprocessableEntity
}

func (s *Server) String() string {
	return fmt.Sprintf("gateway(config=%s)", s.configPath)
}
