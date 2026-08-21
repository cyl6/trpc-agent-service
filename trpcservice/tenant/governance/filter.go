package governance

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cyl6/trpc-agent-service/trpcservice/config"
	"github.com/cyl6/trpc-agent-service/trpcservice/domain"

	"trpc.group/trpc-go/trpc-agent-go/tool"
)

var (
	ErrUserDenied     = errors.New("IM user is not allowed for this tenant binding")
	ErrInputTooLarge  = errors.New("message exceeds tenant input budget")
	ErrRateLimited    = errors.New("tenant request rate exceeded")
	ErrBudgetExceeded = errors.New("tenant monthly model budget exceeded")
	approvalPattern   = regexp.MustCompile(`(?i)(?:^|\s)#approve:([a-f0-9-]{16,64})(?:\s|$)`)
)

const (
	maxAttachmentCount     = 8
	maxAttachmentTypeRunes = 64
	maxAttachmentNameRunes = 256
	maxAttachmentMIMERunes = 128
	maxPendingApprovals    = 10000
	maxLLMCallsPerRun      = 8
)

type requestContextKey struct{}

type RequestContext struct {
	TenantID      string
	ConfigVersion string
	UserID        string
	SessionID     string
	SenderID      string
	Channel       string
	BindingID     string
	AgentName     string
	RequestID     string
	ApprovalNonce string
	AuditPolicy   config.AuditPolicy
	AuditSecrets  []string
}

func WithRequestContext(ctx context.Context, value RequestContext) context.Context {
	return context.WithValue(ctx, requestContextKey{}, value)
}

func RequestContextFrom(ctx context.Context) (RequestContext, bool) {
	value, ok := ctx.Value(requestContextKey{}).(RequestContext)
	return value, ok
}

type minuteBucket struct {
	window int64
	count  int
}

type costBucket struct {
	month    string
	reserved float64
}

// Filter performs inbound user authorization, message-size enforcement and a
// tenant-level fixed-window budget before any model or storage call.
type Filter struct {
	mu      sync.Mutex
	buckets map[string]minuteBucket
	costs   map[string]costBucket
	now     func() time.Time
}

func NewFilter() *Filter {
	return &Filter{buckets: make(map[string]minuteBucket), costs: make(map[string]costBucket), now: time.Now}
}

func (f *Filter) CheckInbound(tenant config.TenantConfig, binding config.ChannelConfig, msg domain.InboundMessage) error {
	if len(binding.AllowedUsers) > 0 && !contains(binding.AllowedUsers, msg.ExternalUserID) {
		return ErrUserDenied
	}
	if err := validateAttachmentMetadata(msg.Attachments); err != nil {
		return err
	}
	prompt := domain.UserPrompt(msg.Text, msg.Attachments, msg.Scope, domain.SenderIdentity(msg))
	if len([]rune(prompt)) > tenant.Budget.MaxInputChars {
		return ErrInputTooLarge
	}
	now := f.now().UTC()
	minute := now.Unix() / 60
	month := now.Format("2006-01")
	estimatedCost := estimateMaxCost(tenant, prompt)
	f.mu.Lock()
	bucket := f.buckets[tenant.TenantID]
	if bucket.window != minute {
		bucket = minuteBucket{window: minute}
	}
	if bucket.count >= tenant.Budget.RequestsPerMinute {
		f.mu.Unlock()
		return ErrRateLimited
	}
	cost := f.costs[tenant.TenantID]
	if cost.month != month {
		cost = costBucket{month: month}
	}
	if tenant.Budget.MonthlyCostUSD > 0 && cost.reserved+estimatedCost > tenant.Budget.MonthlyCostUSD {
		f.mu.Unlock()
		return ErrBudgetExceeded
	}
	bucket.count++
	cost.reserved += estimatedCost
	f.buckets[tenant.TenantID] = bucket
	f.costs[tenant.TenantID] = cost
	f.mu.Unlock()
	return nil
}

func validateAttachmentMetadata(attachments []domain.Attachment) error {
	if len(attachments) > maxAttachmentCount {
		return fmt.Errorf("%w: at most %d attachments are accepted", ErrInputTooLarge, maxAttachmentCount)
	}
	for _, attachment := range attachments {
		if len([]rune(attachment.Type)) > maxAttachmentTypeRunes ||
			len([]rune(attachment.Name)) > maxAttachmentNameRunes ||
			len([]rune(attachment.MimeType)) > maxAttachmentMIMERunes {
			return fmt.Errorf("%w: attachment metadata is too large", ErrInputTooLarge)
		}
	}
	return nil
}

func estimateMaxCost(tenant config.TenantConfig, text string) float64 {
	// Four runes/token remains a tokenizer approximation, but reserve the full
	// configured eight-call Agent envelope rather than only one model turn.
	// Production still settles against provider usage in an atomic ledger.
	inputTokens := (len([]rune(text)) + 3) / 4
	perCall := float64(inputTokens)*tenant.Model.InputPrice + float64(tenant.Model.MaxTokens)*tenant.Model.OutputPrice
	return float64(maxLLMCallsPerRun) * perCall / 1_000_000
}

func ExtractApproval(text string) (cleanText, nonce string) {
	match := approvalPattern.FindStringSubmatch(text)
	if len(match) != 2 {
		return text, ""
	}
	clean := approvalPattern.ReplaceAllString(text, " ")
	return strings.TrimSpace(clean), strings.ToLower(match[1])
}

// ToolFilter is the model-visible allowlist. PermissionPolicy below repeats
// the check at execution time, because visibility is not an authorization
// boundary and framework tools may be injected dynamically.
func ToolFilter(policy config.ToolPolicy) tool.FilterFunc {
	allowed := stringSet(policy.Allow)
	denied := stringSet(policy.Deny)
	return func(_ context.Context, candidate tool.Tool) bool {
		if candidate == nil || candidate.Declaration() == nil {
			return false
		}
		name := candidate.Declaration().Name
		if _, blocked := denied[name]; blocked {
			return false
		}
		_, ok := allowed[name]
		return ok
	}
}

type approval struct {
	scope   string
	expires time.Time
}

type ApprovalStore struct {
	mu    sync.Mutex
	items map[string]approval
	now   func() time.Time
}

func NewApprovalStore() *ApprovalStore {
	return &ApprovalStore{items: make(map[string]approval), now: time.Now}
}

func (s *ApprovalStore) Issue(scope string, ttl time.Duration) (string, error) {
	for attempt := 0; attempt < 3; attempt++ {
		random := make([]byte, 16)
		if _, err := rand.Read(random); err != nil {
			return "", errors.New("generate approval token")
		}
		nonce := hex.EncodeToString(random)
		s.mu.Lock()
		now := s.now()
		s.deleteExpired(now)
		if len(s.items) >= maxPendingApprovals {
			s.mu.Unlock()
			return "", errors.New("approval capacity reached")
		}
		if _, collision := s.items[nonce]; collision {
			s.mu.Unlock()
			continue
		}
		s.items[nonce] = approval{scope: scope, expires: now.Add(ttl)}
		s.mu.Unlock()
		return nonce, nil
	}
	return "", errors.New("generate unique approval token")
}

func (s *ApprovalStore) Consume(nonce, scope string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.deleteExpired(now)
	if nonce == "" {
		return false
	}
	item, ok := s.items[nonce]
	if !ok || item.scope != scope {
		return false
	}
	delete(s.items, nonce)
	return true
}

func (s *ApprovalStore) deleteExpired(now time.Time) {
	for nonce, item := range s.items {
		if !now.Before(item.expires) {
			delete(s.items, nonce)
		}
	}
}

type ToolDecision struct {
	Request       RequestContext
	ToolName      string
	Decision      string
	Reason        string
	ArgumentsHash string
}

type ToolDecisionObserver func(context.Context, ToolDecision)

func PermissionPolicy(policy config.ToolPolicy, approvals *ApprovalStore) tool.PermissionPolicyFunc {
	return PermissionPolicyObserved(policy, approvals, nil)
}

func PermissionPolicyObserved(policy config.ToolPolicy, approvals *ApprovalStore, observer ToolDecisionObserver) tool.PermissionPolicyFunc {
	allowed := stringSet(policy.Allow)
	denied := stringSet(policy.Deny)
	dangerous := stringSet(policy.RequireConfirm)
	return func(ctx context.Context, req *tool.PermissionRequest) (tool.PermissionDecision, error) {
		decide := func(decision tool.PermissionDecision) (tool.PermissionDecision, error) {
			if observer != nil {
				rc, _ := RequestContextFrom(ctx)
				name := ""
				var args []byte
				if req != nil {
					name = req.ToolName
					args = req.Arguments
				}
				observer(ctx, ToolDecision{Request: rc, ToolName: name, Decision: string(decision.Action), Reason: decision.Reason, ArgumentsHash: ArgumentsHash(args)})
			}
			return decision, nil
		}
		if req == nil {
			return decide(tool.DenyPermission("missing tool permission request"))
		}
		if _, blocked := denied[req.ToolName]; blocked {
			return decide(tool.DenyPermission("tool denied by tenant policy"))
		}
		if _, ok := allowed[req.ToolName]; !ok {
			return decide(tool.DenyPermission("tool is not in the tenant allowlist"))
		}
		if _, needsApproval := dangerous[req.ToolName]; !needsApproval {
			return decide(tool.AllowPermission())
		}
		rc, ok := RequestContextFrom(ctx)
		if !ok || approvals == nil {
			return decide(tool.DenyPermission("approval context is unavailable"))
		}
		scope := approvalScope(rc, req.ToolName, req.Arguments)
		if approvals.Consume(rc.ApprovalNonce, scope) {
			return decide(tool.AllowPermission())
		}
		nonce, err := approvals.Issue(scope, 5*time.Minute)
		if err != nil {
			return decide(tool.DenyPermission("approval token is temporarily unavailable"))
		}
		return decide(tool.AskPermission("confirmation required; resend the request with #approve:" + nonce))
	}
}

func approvalScope(rc RequestContext, toolName string, args []byte) string {
	normalized := normalizeJSON(args)
	h := sha256.Sum256(normalized)
	parts := []string{rc.TenantID, rc.ConfigVersion, rc.UserID, rc.SessionID, toolName, hex.EncodeToString(h[:])}
	return strings.Join(parts, "\x1f")
}

func normalizeJSON(raw []byte) []byte {
	var value any
	if json.Unmarshal(raw, &value) == nil {
		if encoded, err := json.Marshal(value); err == nil {
			return encoded
		}
	}
	return raw
}

func ArgumentsHash(raw []byte) string {
	h := sha256.Sum256(normalizeJSON(raw))
	return hex.EncodeToString(h[:])
}

func contains(items []string, value string) bool {
	for _, item := range items {
		if item == value {
			return true
		}
	}
	return false
}

func stringSet(items []string) map[string]struct{} {
	set := make(map[string]struct{}, len(items))
	for _, item := range items {
		set[item] = struct{}{}
	}
	return set
}

// SortedToolNames makes policy decisions deterministic in audit/config views.
func SortedToolNames(policy config.ToolPolicy) []string {
	names := append([]string(nil), policy.Allow...)
	sort.Strings(names)
	return names
}
