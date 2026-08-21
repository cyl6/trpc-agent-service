package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	agentruntime "github.com/cyl6/trpc-agent-service/trpcservice/agent"
	"github.com/cyl6/trpc-agent-service/trpcservice/channels"
	"github.com/cyl6/trpc-agent-service/trpcservice/config"
	"github.com/cyl6/trpc-agent-service/trpcservice/coordination"
	"github.com/cyl6/trpc-agent-service/trpcservice/domain"
	auditlog "github.com/cyl6/trpc-agent-service/trpcservice/log"
	"github.com/cyl6/trpc-agent-service/trpcservice/metrics"
	"github.com/cyl6/trpc-agent-service/trpcservice/tenant/governance"

	"trpc.group/trpc-go/trpc-agent-go/agent"
	"trpc.group/trpc-go/trpc-agent-go/event"
	"trpc.group/trpc-go/trpc-agent-go/model"
)

var tracer = otel.Tracer("trpc-agent-service/worker")

type Task struct {
	Tenant       config.TenantConfig   `json:"tenant"`
	Binding      config.ChannelConfig  `json:"binding"`
	Message      domain.InboundMessage `json:"message"`
	Deliver      bool                  `json:"deliver"`
	TraceCarrier map[string]string     `json:"trace_carrier,omitempty"`
}

type Result struct {
	Text      string `json:"text"`
	RequestID string `json:"request_id"`
	SessionID string `json:"session_id"`
	Duplicate bool   `json:"duplicate"`
}

type pendingResult struct {
	Outbound         domain.OutboundMessage `json:"outbound"`
	RequestID        string                 `json:"request_id"`
	PromptTokens     int                    `json:"prompt_tokens"`
	CompletionTokens int                    `json:"completion_tokens"`
	CostUSD          float64                `json:"cost_usd"`
}

type Service struct {
	runtimes    *agentruntime.Manager
	coordinator coordination.Coordinator
	channels    *channels.Registry
	filter      *governance.Filter
	approvals   *governance.ApprovalStore
	audit       auditlog.Sink
	metrics     *metrics.Metrics
	lockTTL     time.Duration
	dedupTTL    time.Duration
	runTimeout  time.Duration
}

type Options struct {
	LockTTL    time.Duration
	DedupTTL   time.Duration
	RunTimeout time.Duration
}

func NewService(
	runtimes *agentruntime.Manager,
	coordinator coordination.Coordinator,
	channelRegistry *channels.Registry,
	filter *governance.Filter,
	auditSink auditlog.Sink,
	metricsExporter *metrics.Metrics,
	opts Options,
) *Service {
	if opts.LockTTL <= 0 {
		opts.LockTTL = 2 * time.Minute
	}
	if opts.DedupTTL <= 0 {
		opts.DedupTTL = 24 * time.Hour
	}
	if opts.RunTimeout <= 0 {
		opts.RunTimeout = 90 * time.Second
	}
	if runtimes == nil {
		runtimes = agentruntime.NewManager()
	}
	if coordinator == nil {
		coordinator = coordination.NewInMemory()
	}
	if channelRegistry == nil {
		channelRegistry = channels.NewRegistry()
	}
	if filter == nil {
		filter = governance.NewFilter()
	}
	if metricsExporter == nil {
		metricsExporter = metrics.NewMetrics()
	}
	return &Service{
		runtimes: runtimes, coordinator: coordinator, channels: channelRegistry, filter: filter,
		approvals: governance.NewApprovalStore(), audit: auditSink, metrics: metricsExporter,
		lockTTL: opts.LockTTL, dedupTTL: opts.DedupTTL, runTimeout: opts.RunTimeout,
	}
}

func (s *Service) Process(ctx context.Context, task Task) (result Result, err error) {
	started := time.Now()
	msg := task.Message
	msg.TenantID = task.Tenant.TenantID
	msg.BindingID = task.Binding.BindingID
	msg.Channel = task.Binding.Type
	task.Message = msg
	requestID := uuid.NewString()
	result.RequestID = requestID
	principalID, sessionID := domain.Identity(msg, task.Tenant.App.Name)
	result.SessionID = sessionID
	auditUserID := domain.SenderIdentity(msg)
	appNamespace := domain.AppNamespace(task.Tenant.TenantID, task.Tenant.App.Name)
	dedupKey := messageKey(msg)
	labels := map[string]string{"tenant": task.Tenant.TenantID, "channel": msg.Channel}
	ctx, span := tracer.Start(ctx, "worker.process", trace.WithAttributes(
		attribute.String("tenant.id", task.Tenant.TenantID),
		attribute.String("channel", msg.Channel),
		attribute.String("config.revision", task.Tenant.Version),
	))
	defer func() {
		if err != nil {
			span.SetStatus(codes.Error, "request_failed:"+errorType(err))
		}
		span.End()
		s.metrics.Add("agent_requests_total", "Agent messages processed.", 1, mergeLabels(labels, "result", resultLabel(err)))
		s.metrics.Observe("agent_request_latency_seconds", "End-to-end worker latency.", time.Since(started).Seconds(), labels)
	}()

	lockCtx, cancelLock := context.WithTimeout(ctx, s.processingTTL())
	lockLease, err := s.coordinator.Lock(lockCtx, appNamespace+":"+principalID+":"+sessionID, s.lockTTL)
	cancelLock()
	if err != nil {
		return result, err
	}
	defer lockLease.Release()
	processCtx, cancelProcess := context.WithCancelCause(ctx)
	defer cancelProcess(nil)
	go func() {
		select {
		case lostErr := <-lockLease.Lost:
			if lostErr != nil {
				cancelProcess(lostErr)
			}
		case <-processCtx.Done():
		}
	}()
	ctx = processCtx

	claimLease, err := s.coordinator.Claim(ctx, dedupKey, s.processingTTL())
	if err != nil {
		return result, err
	}
	if claimLease.State == coordination.AlreadyCompleted {
		result.Duplicate = true
		return result, nil
	}
	if claimLease.State == coordination.AlreadyProcessing {
		// The full session lane is already held, so this is an orphaned or
		// independently active claim rather than a completed duplicate.
		return result, coordination.ErrClaimInProgress
	}
	claimCompleted := false
	defer func() {
		if !claimCompleted {
			_ = s.coordinator.ReleaseClaim(context.Background(), dedupKey, claimLease.Token)
		}
	}()

	var pending pendingResult
	if found, loadErr := s.coordinator.LoadResult(ctx, dedupKey, &pending); loadErr != nil {
		return result, loadErr
	} else if found {
		result.Text = pending.Outbound.Text
		if pending.RequestID != "" {
			result.RequestID = pending.RequestID
		}
		if task.Deliver {
			if err := s.deliver(ctx, task.Binding, pending.Outbound); err != nil {
				s.metrics.Add("im_delivery_total", "IM delivery attempts.", 1, mergeLabels(labels, "result", "error"))
				return result, err
			}
			s.metrics.Add("im_delivery_total", "IM delivery attempts.", 1, mergeLabels(labels, "result", "success"))
		}
		if err := s.finishClaim(ctx, dedupKey, claimLease.Token); err != nil {
			return result, err
		}
		claimCompleted = true
		return result, nil
	}

	if err := s.filter.CheckInbound(task.Tenant, task.Binding, msg); err != nil {
		completeErr := s.coordinator.Complete(ctx, dedupKey, claimLease.Token, s.dedupTTL)
		claimCompleted = completeErr == nil
		s.writeAudit(ctx, auditEntryForTenant(auditlog.Entry{
			Timestamp: time.Now().UTC(), TenantID: task.Tenant.TenantID, Channel: msg.Channel,
			BindingID: msg.BindingID, UserID: auditUserID, SessionID: sessionID,
			AgentName: task.Tenant.App.AgentName, Decision: "deny", Reason: err.Error(),
			LatencyMS: time.Since(started).Milliseconds(), ErrorType: errorType(err),
			RequestID: requestID, ConfigRevision: task.Tenant.Version, ContentHash: auditlog.ContentHash(msg.Text),
		}, task.Tenant))
		if completeErr != nil {
			return result, errors.Join(err, completeErr)
		}
		return result, err
	}

	runtime, release, err := s.runtimes.Acquire(ctx, task.Tenant)
	if err != nil {
		return result, err
	}
	defer release()

	cleanText, approvalNonce := governance.ExtractApproval(msg.Text)
	runCtx, cancel := context.WithTimeout(ctx, s.runTimeout)
	defer cancel()
	runCtx = governance.WithRequestContext(runCtx, governance.RequestContext{
		TenantID: task.Tenant.TenantID, ConfigVersion: task.Tenant.Version,
		UserID: auditUserID, SessionID: sessionID, SenderID: auditUserID,
		Channel: msg.Channel, BindingID: msg.BindingID, AgentName: task.Tenant.App.AgentName,
		RequestID: requestID, ApprovalNonce: approvalNonce,
		AuditPolicy: task.Tenant.Audit, AuditSecrets: task.Tenant.SecretEnvNames(),
	})
	events, err := runtime.Runner.Run(
		runCtx,
		principalID,
		sessionID,
		buildUserMessage(cleanText, msg.Attachments, msg.Scope, auditUserID),
		agent.WithAppName(runtime.AppNamespace),
		agent.WithRequestID(requestID),
		agent.WithRuntimeState(map[string]any{
			"tenant_id": task.Tenant.TenantID, "config_revision": task.Tenant.Version,
			"channel": msg.Channel, "sender_id": auditUserID,
		}),
		agent.WithKnowledgeFilter(map[string]any{"tenant_id": task.Tenant.TenantID, "app_name": task.Tenant.App.Name}),
		agent.WithToolFilter(governance.ToolFilter(task.Tenant.Tools)),
		agent.WithToolPermissionPolicyFunc(governance.PermissionPolicyObserved(task.Tenant.Tools, s.approvals, s.observeToolDecision)),
		agent.WithMaxRunDuration(s.runTimeout),
		agent.WithSpanAttributes(
			attribute.String("tenant.id", task.Tenant.TenantID),
			attribute.String("channel", msg.Channel),
		),
	)
	if err != nil {
		return result, fmt.Errorf("run agent: %w", err)
	}
	text, promptTokens, completionTokens, err := collect(events)
	if err != nil {
		return result, err
	}
	if strings.TrimSpace(text) == "" {
		text = "消息已处理，但 Agent 没有返回可显示的文本。"
	}
	result.Text = text
	cost := (float64(promptTokens)*task.Tenant.Model.InputPrice + float64(completionTokens)*task.Tenant.Model.OutputPrice) / 1_000_000
	outbound := domain.OutboundMessage{
		TenantID: task.Tenant.TenantID, BindingID: task.Binding.BindingID, Channel: task.Binding.Type,
		Target: msg.ReplyTarget, ThreadID: msg.ThreadID, Scope: msg.Scope, Text: text,
	}
	pending = pendingResult{
		Outbound: outbound, RequestID: requestID,
		PromptTokens: promptTokens, CompletionTokens: completionTokens, CostUSD: cost,
	}
	if err := s.coordinator.SaveResult(ctx, dedupKey, claimLease.Token, pending, s.dedupTTL); err != nil {
		return result, err
	}
	// Agent completion accounting is independent of IM delivery. Record it as
	// soon as the replayable result exists, so a transient send failure does not
	// erase model usage or the allow audit entry.
	s.metrics.Add("model_tokens_total", "Model tokens consumed.", float64(promptTokens+completionTokens), labels)
	s.metrics.Add("tenant_cost_usd_total", "Estimated model cost in USD.", cost, labels)
	s.writeAudit(ctx, auditEntryForTenant(auditlog.Entry{
		Timestamp: time.Now().UTC(), TenantID: task.Tenant.TenantID, Channel: msg.Channel,
		BindingID: msg.BindingID, UserID: auditUserID, SessionID: sessionID,
		AgentName: task.Tenant.App.AgentName, Decision: "allow", LatencyMS: time.Since(started).Milliseconds(),
		CostUSD: cost, RequestID: requestID, ConfigRevision: task.Tenant.Version,
		ContentHash: auditlog.ContentHash(msg.Text),
	}, task.Tenant))
	if task.Deliver {
		if err := s.deliver(ctx, task.Binding, outbound); err != nil {
			s.metrics.Add("im_delivery_total", "IM delivery attempts.", 1, mergeLabels(labels, "result", "error"))
			return result, err
		}
		s.metrics.Add("im_delivery_total", "IM delivery attempts.", 1, mergeLabels(labels, "result", "success"))
	}
	if err := s.finishClaim(ctx, dedupKey, claimLease.Token); err != nil {
		return result, err
	}
	claimCompleted = true
	return result, nil
}

func buildUserMessage(text string, attachments []domain.Attachment, scope domain.Scope, senderID string) model.Message {
	// Runner uses a stable group principal. UserPrompt preserves the
	// pseudonymous human sender and safe attachment metadata without forwarding
	// provider URLs or file IDs.
	return model.NewUserMessage(domain.UserPrompt(text, attachments, scope, senderID))
}

func (s *Service) observeToolDecision(ctx context.Context, decision governance.ToolDecision) {
	s.metrics.Add("tool_permission_total", "Tool permission decisions.", 1, map[string]string{
		"tenant": decision.Request.TenantID, "component": decision.ToolName, "result": decision.Decision,
	})
	policy := decision.Request.AuditPolicy
	s.writeAudit(ctx, auditlog.Entry{
		Timestamp: time.Now().UTC(), TenantID: decision.Request.TenantID,
		Channel: decision.Request.Channel, BindingID: decision.Request.BindingID,
		UserID: decision.Request.UserID, SessionID: decision.Request.SessionID,
		AgentName: decision.Request.AgentName, ToolName: decision.ToolName,
		Decision: decision.Decision, Reason: decision.Reason,
		RequestID: decision.Request.RequestID, ConfigRevision: decision.Request.ConfigVersion,
		ToolArgsHash:   decision.ArgumentsHash,
		PolicySnapshot: &policy, SecretEnvNames: append([]string(nil), decision.Request.AuditSecrets...),
	})
}

func (s *Service) finishClaim(ctx context.Context, key, ownerToken string) error {
	if err := s.coordinator.Complete(ctx, key, ownerToken, s.dedupTTL); err != nil {
		return err
	}
	return s.coordinator.DeleteResult(ctx, key)
}

func (s *Service) processingTTL() time.Duration {
	ttl := 2 * s.runTimeout
	if lockWindow := 2 * s.lockTTL; ttl < lockWindow {
		ttl = lockWindow
	}
	if ttl <= 0 {
		return 3 * time.Minute
	}
	return ttl
}

func (s *Service) deliver(ctx context.Context, binding config.ChannelConfig, outbound domain.OutboundMessage) error {
	adapter, err := s.channels.Get(binding.Type)
	if err != nil {
		return err
	}
	ctx, span := tracer.Start(ctx, "im.send", trace.WithAttributes(attribute.String("channel", binding.Type)))
	defer span.End()
	if err := adapter.Deliver(ctx, binding, outbound); err != nil {
		span.SetStatus(codes.Error, "delivery failed")
		return fmt.Errorf("deliver IM response: %w", err)
	}
	return nil
}

func collect(events <-chan *event.Event) (text string, promptTokens, completionTokens int, err error) {
	var streamed strings.Builder
	var full string
	var completionText string
	var terminalErr error
	sawCompletion := false
	for evt := range events {
		if evt == nil {
			continue
		}
		isRoot := evt.ParentInvocationID == ""
		if isRoot && evt.IsTerminalError() && terminalErr == nil {
			terminalErr = errors.New(evt.Error.Message)
		}
		if evt.Response != nil && evt.Usage != nil {
			if evt.IsRunnerCompletion() {
				// Runner completion carries the aggregate usage for the run.
				promptTokens = evt.Usage.PromptTokens
				completionTokens = evt.Usage.CompletionTokens
			} else {
				promptTokens += evt.Usage.PromptTokens
				completionTokens += evt.Usage.CompletionTokens
			}
		}
		if evt.Response != nil && isRoot {
			for _, choice := range evt.Choices {
				if !evt.IsRunnerCompletion() && choice.Delta.Content != "" {
					streamed.WriteString(choice.Delta.Content)
				}
				if choice.Message.Role == model.RoleAssistant && choice.Message.Content != "" {
					if evt.IsRunnerCompletion() {
						completionText = choice.Message.Content
					} else {
						full = choice.Message.Content
					}
				}
			}
		}
		if evt.IsRunnerCompletion() {
			sawCompletion = true
			if evt.Error != nil && terminalErr == nil {
				terminalErr = errors.New(evt.Error.Message)
			}
		}
	}
	if terminalErr != nil {
		return "", promptTokens, completionTokens, terminalErr
	}
	if !sawCompletion {
		return "", promptTokens, completionTokens, errors.New("runner event stream closed before completion")
	}
	if completionText != "" {
		return completionText, promptTokens, completionTokens, nil
	}
	if streamed.Len() > 0 {
		return streamed.String(), promptTokens, completionTokens, nil
	}
	return full, promptTokens, completionTokens, nil
}

func messageKey(msg domain.InboundMessage) string {
	h := sha256.Sum256([]byte(strings.Join([]string{msg.TenantID, msg.BindingID, msg.Channel, msg.ExternalMessageID}, "\x1f")))
	return hex.EncodeToString(h[:])
}

func (s *Service) writeAudit(ctx context.Context, entry auditlog.Entry) {
	if s.audit == nil {
		return
	}
	spanContext := trace.SpanContextFromContext(ctx)
	if spanContext.IsValid() {
		entry.TraceID = spanContext.TraceID().String()
	}
	if err := s.audit.Write(entry); err != nil {
		s.metrics.Add("audit_write_failures_total", "Audit records that could not be written.", 1, map[string]string{
			"tenant": entry.TenantID,
		})
	}
}

func auditEntryForTenant(entry auditlog.Entry, tenantConfig config.TenantConfig) auditlog.Entry {
	policy := tenantConfig.Audit
	entry.PolicySnapshot = &policy
	entry.SecretEnvNames = tenantConfig.SecretEnvNames()
	return entry
}

func mergeLabels(base map[string]string, key, value string) map[string]string {
	result := make(map[string]string, len(base)+1)
	for k, v := range base {
		result[k] = v
	}
	result[key] = value
	return result
}

func resultLabel(err error) string {
	if err != nil {
		return "error"
	}
	return "success"
}

func errorType(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, governance.ErrUserDenied) {
		return "permission_denied"
	}
	if errors.Is(err, governance.ErrRateLimited) {
		return "rate_limited"
	}
	if errors.Is(err, governance.ErrBudgetExceeded) {
		return "budget_exceeded"
	}
	if errors.Is(err, governance.ErrInputTooLarge) {
		return "input_too_large"
	}
	return "internal"
}
