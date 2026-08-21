package channels

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cyl6/trpc-agent-service/trpcservice/config"
	"github.com/cyl6/trpc-agent-service/trpcservice/domain"
)

type leakingErrorDoer struct{}

func (leakingErrorDoer) Do(req *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("network failure for %s", req.URL.String())
}

type captureDoer struct {
	payload map[string]any
	reply   string
}

func (d *captureDoer) Do(req *http.Request) (*http.Response, error) {
	if err := json.NewDecoder(req.Body).Decode(&d.payload); err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(d.reply)),
		Header:     make(http.Header),
	}, nil
}

func TestTelegramVerifyAndParse(t *testing.T) {
	t.Setenv("TG_SECRET", "hook-secret")
	binding := config.ChannelConfig{Type: "telegram", BindingID: "main", SigningSecretEnv: "TG_SECRET"}
	body := []byte(`{"update_id":42,"message":{"message_id":3,"date":1700000000,"text":"hello","from":{"id":7},"chat":{"id":9,"type":"private"}}}`)
	req := httptest.NewRequest("POST", "/", bytes.NewReader(body))
	req.Header.Set(telegramSecretHeader, os.Getenv("TG_SECRET"))
	adapter := NewTelegram(nil)
	if err := adapter.Verify(req, body, binding); err != nil {
		t.Fatal(err)
	}
	parsed, err := adapter.Parse(body, binding)
	if err != nil {
		t.Fatal(err)
	}
	msg := parsed.Messages[0]
	if msg.Scope != domain.ScopeDirect || msg.ExternalMessageID != "42" || msg.ExternalUserID != "7" {
		t.Fatalf("unexpected normalized message: %+v", msg)
	}
}

func TestSlackVerifyParseAndRejectReplay(t *testing.T) {
	t.Setenv("SLACK_SECRET", "signing-secret")
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"type":"event_callback","team_id":"T1","api_app_id":"A1","event_id":"Ev1","event_time":1700000000,"event":{"type":"message","user":"U1","text":"hello","channel":"C1","channel_type":"channel","ts":"1.2"}}`)
	ts := strconv.FormatInt(now.Unix(), 10)
	mac := hmac.New(sha256.New, []byte("signing-secret"))
	_, _ = mac.Write([]byte("v0:" + ts + ":" + string(body)))
	signature := "v0=" + hex.EncodeToString(mac.Sum(nil))
	req := httptest.NewRequest("POST", "/", bytes.NewReader(body))
	req.Header.Set("X-Slack-Request-Timestamp", ts)
	req.Header.Set("X-Slack-Signature", signature)
	binding := config.ChannelConfig{Type: "slack", BindingID: "main", SigningSecretEnv: "SLACK_SECRET", WorkspaceID: "T1", ApplicationID: "A1"}
	adapter := NewSlack(nil)
	adapter.now = func() time.Time { return now }
	if err := adapter.Verify(req, body, binding); err != nil {
		t.Fatal(err)
	}
	parsed, err := adapter.Parse(body, binding)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Messages[0].Scope != domain.ScopeGroup || parsed.Messages[0].ExternalMessageID != "Ev1" || parsed.Messages[0].ThreadID != "1.2" {
		t.Fatalf("unexpected normalized message: %+v", parsed.Messages[0])
	}
	mismatched := binding
	mismatched.WorkspaceID = "T2"
	if _, err := adapter.Parse(body, mismatched); err != ErrBindingMismatch {
		t.Fatalf("expected Slack workspace mismatch rejection, got %v", err)
	}
	adapter.now = func() time.Time { return now.Add(6 * time.Minute) }
	if err := adapter.Verify(req, body, binding); err != ErrInvalidSignature {
		t.Fatalf("expected stale request rejection, got %v", err)
	}
}

func TestTelegramTopicParticipatesInNormalizedThread(t *testing.T) {
	body := []byte(`{"update_id":43,"message":{"message_id":4,"message_thread_id":99,"date":1700000000,"text":"topic","from":{"id":7},"chat":{"id":-9,"type":"supergroup"}}}`)
	parsed, err := NewTelegram(nil).Parse(body, config.ChannelConfig{BindingID: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.Messages[0]; got.Scope != domain.ScopeGroup || got.ThreadID != "99" {
		t.Fatalf("telegram topic was not normalized: %+v", got)
	}
}

func TestChunksUsesRuneLength(t *testing.T) {
	got := chunks("你好世界", 3)
	if len(got) != 2 || got[0] != "你好世" || got[1] != "界" {
		t.Fatalf("unexpected chunks: %#v", got)
	}
}

func TestTelegramDeliveryErrorNeverLeaksBotToken(t *testing.T) {
	t.Setenv("TG_TOKEN", "123456:super-secret-bot-token")
	binding := config.ChannelConfig{TokenEnv: "TG_TOKEN", MaxMessageLength: 4096}
	err := NewTelegram(leakingErrorDoer{}).Deliver(context.Background(), binding, domain.OutboundMessage{Target: "1", Text: "hello"})
	if err == nil {
		t.Fatal("expected delivery failure")
	}
	if strings.Contains(err.Error(), os.Getenv("TG_TOKEN")) {
		t.Fatalf("bot token leaked in error: %v", err)
	}
}

func TestSlackAndTelegramDeliverPreserveThread(t *testing.T) {
	t.Setenv("SLACK_TOKEN", "slack-test-token")
	slackHTTP := &captureDoer{reply: `{"ok":true}`}
	err := NewSlack(slackHTTP).Deliver(context.Background(), config.ChannelConfig{
		TokenEnv: "SLACK_TOKEN", APIBaseURL: "https://slack.invalid/api", MaxMessageLength: 100,
	}, domain.OutboundMessage{Target: "C1", ThreadID: "123.45", Text: "reply"})
	if err != nil || slackHTTP.payload["thread_ts"] != "123.45" {
		t.Fatalf("Slack thread payload = %#v, err=%v", slackHTTP.payload, err)
	}

	t.Setenv("TG_TOKEN", "telegram-test-token")
	telegramHTTP := &captureDoer{reply: `{"ok":true}`}
	err = NewTelegram(telegramHTTP).Deliver(context.Background(), config.ChannelConfig{
		TokenEnv: "TG_TOKEN", APIBaseURL: "https://telegram.invalid", MaxMessageLength: 100,
	}, domain.OutboundMessage{Target: "-1", ThreadID: "99", Text: "reply"})
	if err != nil || telegramHTTP.payload["message_thread_id"] != float64(99) {
		t.Fatalf("Telegram topic payload = %#v, err=%v", telegramHTTP.payload, err)
	}
}
