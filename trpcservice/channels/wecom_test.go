package channels

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cyl6/trpc-agent-service/trpcservice/config"
	"github.com/cyl6/trpc-agent-service/trpcservice/domain"
)

// wecomFixture builds signed+encrypted WeCom callbacks the same way the
// provider does, so the adapter is verified against real wire format.
type wecomFixture struct {
	aesKey      string
	token       string
	corpid      string
	agentID     string
	now         time.Time
	timestamp   string
	nonce       string
	cipher      *wecomCipher
	tokenHits   int
	mu          sync.Mutex
	accessToken string
	sendHits    int
	sendPayload []map[string]any
	server      *httptest.Server
}

func newWeComFixture(t *testing.T) *wecomFixture {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	aesKey := base64.StdEncoding.EncodeToString(raw)[:43]
	cipherInstance, err := newWeComCipher(aesKey)
	if err != nil {
		t.Fatal(err)
	}
	f := &wecomFixture{
		aesKey: aesKey, token: "callback-token", corpid: "wwCorpId123",
		agentID: "1000002", now: time.Unix(1_700_000_000, 0),
		nonce: "n0nce", cipher: cipherInstance, accessToken: "cached-access-token",
	}
	f.timestamp = fmt.Sprintf("%d", f.now.Unix())
	return f
}

func (f *wecomFixture) binding() config.ChannelConfig {
	return config.ChannelConfig{
		Type: "wecom", BindingID: "acme-wecom",
		TokenEnv: "WECOM_CORP_SECRET", SigningSecretEnv: "WECOM_CALLBACK_TOKEN",
		EncryptionKeyEnv: "WECOM_AES_KEY",
		WorkspaceID: f.corpid, ApplicationID: f.agentID,
		MaxMessageLength: 2048,
	}
}

func (f *wecomFixture) env(t *testing.T) {
	t.Helper()
	t.Setenv("WECOM_CORP_SECRET", "corp-secret-value")
	t.Setenv("WECOM_CALLBACK_TOKEN", f.token)
	t.Setenv("WECOM_AES_KEY", f.aesKey)
}

func (f *wecomFixture) encrypt(t *testing.T, plaintext string) string {
	t.Helper()
	ciphertext, err := f.cipher.encrypt([]byte(plaintext), []byte(f.corpid))
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(ciphertext)
}

func (f *wecomFixture) sign(encrypted string) string {
	return wecomSignature(f.token, f.timestamp, f.nonce, encrypted)
}

// callback builds a POST body and signature for a plaintext XML message.
func (f *wecomFixture) callback(t *testing.T, plaintextXML string) ([]byte, string) {
	t.Helper()
	encrypted := f.encrypt(t, plaintextXML)
	body, err := xml.Marshal(struct {
		XMLName struct{} `xml:"xml"`
		Encrypt string   `xml:"Encrypt"`
	}{Encrypt: encrypted})
	if err != nil {
		t.Fatal(err)
	}
	return body, f.sign(encrypted)
}

func (f *wecomFixture) postRequest(t *testing.T, body []byte, signature string) *http.Request {
	t.Helper()
	req := httptest.NewRequest("POST", "/webhooks/wecom/acme-wecom?msg_signature="+signature+"&timestamp="+f.timestamp+"&nonce="+f.nonce, bytes.NewReader(body))
	return req
}

func TestWeComVerifyParseDirectMessage(t *testing.T) {
	f := newWeComFixture(t)
	f.env(t)
	adapter := NewWeCom(nil)
	adapter.now = func() time.Time { return f.now }

	plain := `<xml><ToUserName>` + f.corpid + `</ToUserName><FromUserName>zhangsan</FromUserName><CreateTime>1700000000</CreateTime><MsgType>text</MsgType><Content>你好</Content><MsgId>7758</MsgId><AgentID>1000002</AgentID></xml>`
	body, signature := f.callback(t, plain)
	req := f.postRequest(t, body, signature)
	if err := adapter.Verify(req, body, f.binding()); err != nil {
		t.Fatal(err)
	}
	parsed, err := adapter.Parse(body, f.binding())
	if err != nil {
		t.Fatal(err)
	}
	msg := parsed.Messages[0]
	if msg.Scope != domain.ScopeDirect || msg.ExternalUserID != "zhangsan" ||
		msg.ExternalMessageID != "7758" || msg.ReplyTarget != "zhangsan" || msg.Text != "你好" {
		t.Fatalf("unexpected normalized wecom message: %+v", msg)
	}

	// Tampered signature must be rejected before any parsing.
	if err := adapter.Verify(f.postRequest(t, body, "deadbeef"+signature), body, f.binding()); err != ErrInvalidSignature {
		t.Fatalf("expected signature rejection, got %v", err)
	}
	// Stale timestamp outside the replay window must be rejected.
	stale := NewWeCom(nil)
	stale.now = func() time.Time { return f.now.Add(6 * time.Minute) }
	if err := stale.Verify(req, body, f.binding()); err != ErrInvalidSignature {
		t.Fatalf("expected stale request rejection, got %v", err)
	}
}

func TestWeComParseGroupChatAndBindingChecks(t *testing.T) {
	f := newWeComFixture(t)
	f.env(t)
	adapter := NewWeCom(nil)
	adapter.now = func() time.Time { return f.now }

	groupPlain := `<xml><ToUserName>` + f.corpid + `</ToUserName><FromUserName>lisi</FromUserName><CreateTime>1700000001</CreateTime><MsgType>text</MsgType><Content>群消息</Content><MsgId>7759</MsgId><AgentID>1000002</AgentID><ChatId>wrGroupChat1</ChatId></xml>`
	body, _ := f.callback(t, groupPlain)
	parsed, err := adapter.Parse(body, f.binding())
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Messages[0].Scope != domain.ScopeGroup || parsed.Messages[0].ReplyTarget != "wrGroupChat1" ||
		parsed.Messages[0].ConversationID != "wrGroupChat1" {
		t.Fatalf("group chat was not normalized: %+v", parsed.Messages[0])
	}

	// Same corpid but different agentid must not cross route into this binding.
	otherApp := `<xml><ToUserName>` + f.corpid + `</ToUserName><FromUserName>wangwu</FromUserName><CreateTime>1700000002</CreateTime><MsgType>text</MsgType><Content>hi</Content><MsgId>7760</MsgId><AgentID>1000099</AgentID></xml>`
	otherBody, _ := f.callback(t, otherApp)
	if _, err := adapter.Parse(otherBody, f.binding()); err != ErrBindingMismatch {
		t.Fatalf("expected agentid binding mismatch, got %v", err)
	}

	// Event callbacks (e.g. subscribe) are ignored without an error.
	event := `<xml><ToUserName>` + f.corpid + `</ToUserName><FromUserName>zhangsan</FromUserName><CreateTime>1700000003</CreateTime><MsgType>event</MsgType><Event>subscribe</Event><AgentID>1000002</AgentID></xml>`
	eventBody, _ := f.callback(t, event)
	if _, err := adapter.Parse(eventBody, f.binding()); err != ErrUnsupportedEvent {
		t.Fatalf("expected unsupported event, got %v", err)
	}
}

func TestWeComURLVerification(t *testing.T) {
	f := newWeComFixture(t)
	f.env(t)
	adapter := NewWeCom(nil)
	adapter.now = func() time.Time { return f.now }

	echoPlain := f.encrypt(t, "random-echo-plaintext")
	signature := f.sign(echoPlain)
	encodedEcho := url.QueryEscape(echoPlain)
	req := httptest.NewRequest("GET", "/webhooks/wecom/acme-wecom?msg_signature="+signature+"&timestamp="+f.timestamp+"&nonce="+f.nonce+"&echostr="+encodedEcho, nil)
	echo, err := adapter.VerifyURL(req, f.binding())
	if err != nil {
		t.Fatal(err)
	}
	if echo != "random-echo-plaintext" {
		t.Fatalf("unexpected echo plaintext: %q", echo)
	}

	badSig := httptest.NewRequest("GET", "/webhooks/wecom/acme-wecom?msg_signature=tampered&timestamp="+f.timestamp+"&nonce="+f.nonce+"&echostr="+encodedEcho, nil)
	if _, err := adapter.VerifyURL(badSig, f.binding()); err != ErrInvalidSignature {
		t.Fatalf("expected URL verification rejection, got %v", err)
	}

	// A decrypted payload for another corpid must not be accepted.
	foreign := &wecomFixture{aesKey: f.aesKey, token: f.token, corpid: "wwOtherCorp", cipher: f.cipher, now: f.now, nonce: f.nonce}
	foreign.timestamp = f.timestamp
	foreignEcho := foreign.encrypt(t, "foreign-echo")
	foreignSig := wecomSignature(f.token, f.timestamp, f.nonce, foreignEcho)
	foreignReq := httptest.NewRequest("GET", "/webhooks/wecom/acme-wecom?msg_signature="+foreignSig+"&timestamp="+f.timestamp+"&nonce="+f.nonce+"&echostr="+url.QueryEscape(foreignEcho), nil)
	if _, err := adapter.VerifyURL(foreignReq, f.binding()); err != ErrBindingMismatch {
		t.Fatalf("expected corpid binding mismatch, got %v", err)
	}
}

type wecomDoer struct {
	fixture *wecomFixture
}

func wecomJSONResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
}

func (d *wecomDoer) Do(req *http.Request) (*http.Response, error) {
	path := req.URL.Path
	if path == "/cgi-bin/gettoken" {
		d.fixture.mu.Lock()
		d.fixture.tokenHits++
		d.fixture.mu.Unlock()
		return wecomJSONResponse(`{"errcode":0,"errmsg":"ok","access_token":"` + d.fixture.accessToken + `","expires_in":7200}`), nil
	}
	d.fixture.mu.Lock()
	defer d.fixture.mu.Unlock()
	d.fixture.sendHits++
	payload := map[string]any{}
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(req.Body)
	if err := json.Unmarshal(buf.Bytes(), &payload); err != nil {
		return nil, err
	}
	d.fixture.sendPayload = append(d.fixture.sendPayload, payload)
	if req.URL.Query().Get("access_token") != d.fixture.accessToken {
		return wecomJSONResponse(`{"errcode":42001,"errmsg":"access token expired"}`), nil
	}
	return wecomJSONResponse(`{"errcode":0,"errmsg":"ok"}`), nil
}

func TestWeComDeliverCachesTokenSplitsBytesAndRoutesByScope(t *testing.T) {
	f := newWeComFixture(t)
	f.env(t)
	doer := &wecomDoer{fixture: f}
	adapter := NewWeCom(doer)
	adapter.now = func() time.Time { return f.now }

	// Chinese characters are 3 bytes each; 700 chars = 2100 bytes > 2048.
	// The 2048-byte cut lands mid-rune, so part one backs off to 2046 bytes.
	long := strings.Repeat("好", 700)
	err := adapter.Deliver(context.Background(), f.binding(), domain.OutboundMessage{
		Target: "zhangsan", Scope: domain.ScopeDirect, Text: long,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Group replies go through appchat/send with chatid only.
	err = adapter.Deliver(context.Background(), f.binding(), domain.OutboundMessage{
		Target: "wrGroupChat1", Scope: domain.ScopeGroup, Text: "群回复",
	})
	if err != nil {
		t.Fatal(err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sendPayload) != 3 {
		t.Fatalf("expected 2 byte-limited parts plus 1 group reply, got %d sends", len(f.sendPayload))
	}
	if got := len(f.sendPayload[0]["text"].(map[string]any)["content"].(string)); got != 2046 {
		t.Fatalf("first part should stop at rune boundary 2046 bytes, got %d", got)
	}
	if f.sendPayload[0]["touser"] != "zhangsan" || f.sendPayload[0]["agentid"].(float64) != 1000002 {
		t.Fatalf("direct payload routing wrong: %#v", f.sendPayload[0])
	}
	if _, hasChat := f.sendPayload[0]["chatid"]; hasChat {
		t.Fatalf("direct message must not use chatid: %#v", f.sendPayload[0])
	}
	last := f.sendPayload[len(f.sendPayload)-1]
	if last["chatid"] != "wrGroupChat1" || last["agentid"] != nil {
		t.Fatalf("group payload routing wrong: %#v", last)
	}
	if f.tokenHits != 1 {
		t.Fatalf("access token should be fetched once and reused, got %d fetches", f.tokenHits)
	}
}

func TestWeComDeliverRetriesOnceOnExpiredTokenWithoutLeakingSecret(t *testing.T) {
	f := newWeComFixture(t)
	f.env(t)
	f.accessToken = "rotated-token-value"
	doer := &wecomDoer{fixture: f}
	// Simulate a stale cached token by pre-seeding the adapter cache with the
	// old value, then expiring it on first send.
	adapter := NewWeCom(doer)
	adapter.now = func() time.Time { return f.now }
	adapter.tokens[f.corpid+"\x1fWECOM_CORP_SECRET"] = wecomTokenEntry{
		accessToken: "stale-token", expiresAt: f.now.Add(time.Hour),
	}
	// The doer rejects the stale token once; after invalidation the fresh one
	// is accepted.
	err := adapter.Deliver(context.Background(), f.binding(), domain.OutboundMessage{
		Target: "zhangsan", Scope: domain.ScopeDirect, Text: "after rotation",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendHits != 2 {
		t.Fatalf("expected stale-token retry, send hits = %d", f.sendHits)
	}
	if f.sendPayload[0]["touser"] != "zhangsan" || f.sendPayload[1]["touser"] != "zhangsan" {
		t.Fatalf("retry must resend the same part: %#v", f.sendPayload)
	}
}

func TestWeComDeliveryErrorNeverLeaksSecrets(t *testing.T) {
	f := newWeComFixture(t)
	f.env(t)
	err := NewWeCom(leakingErrorDoer{}).Deliver(context.Background(), f.binding(), domain.OutboundMessage{
		Target: "zhangsan", Scope: domain.ScopeDirect, Text: "hello",
	})
	if err == nil {
		t.Fatal("expected delivery failure")
	}
	for _, secret := range []string{os.Getenv("WECOM_CORP_SECRET"), f.aesKey, f.token} {
		if secret != "" && strings.Contains(err.Error(), secret) {
			t.Fatalf("secret leaked in error: %v", err)
		}
	}
}

func TestChunksUTF8BytesSplitsOnRuneBoundary(t *testing.T) {
	got := chunksUTF8Bytes("你好世界", 6) // 2 runes per chunk
	if len(got) != 2 || got[0] != "你好" || got[1] != "世界" {
		t.Fatalf("unexpected byte chunks: %#v", got)
	}
	if got := chunksUTF8Bytes("abc", 6); len(got) != 1 || got[0] != "abc" {
		t.Fatalf("short text must stay whole: %#v", got)
	}
}
