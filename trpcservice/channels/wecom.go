package channels

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cyl6/trpc-agent-service/trpcservice/config"
	"github.com/cyl6/trpc-agent-service/trpcservice/domain"
)

// WeCom implements the adapter contract for 企业微信 (WeCom) self-built
// applications. Unlike Telegram/Slack, WeCom encrypts callback payloads with
// AES-256-CBC and authenticates them with an SHA1 signature over
// (token, timestamp, nonce, ciphertext). Replies are sent proactively through
// the message/send API, so the callback only needs to be ACKed quickly.
type WeCom struct {
	client HTTPDoer
	now    func() time.Time

	mu     sync.Mutex
	tokens map[string]wecomTokenEntry
}

type wecomTokenEntry struct {
	accessToken string
	expiresAt   time.Time
}

const (
	wecomDefaultAPIBase = "https://qyapi.weixin.qq.com"
	wecomMaxClockSkew   = 5 * time.Minute
	// WeCom rejects text messages beyond 2048 UTF-8 bytes.
	wecomDefaultMessageBytes = 2048
)

func NewWeCom(client HTTPDoer) *WeCom {
	if client == nil {
		client = http.DefaultClient
	}
	return &WeCom{client: client, now: time.Now, tokens: make(map[string]wecomTokenEntry)}
}

func (*WeCom) Name() string { return "wecom" }

// wecomCipher implements the WeCom callback encryption scheme: AES-256-CBC
// with IV = key[:16] and PKCS#7 padding over random(16) || len(4) || msg || receiveID.
type wecomCipher struct{ key []byte }

func newWeComCipher(encodingAESKey string) (*wecomCipher, error) {
	if len(encodingAESKey) != 43 {
		return nil, errors.New("invalid wecom encoding aes key length")
	}
	key, err := base64.StdEncoding.DecodeString(encodingAESKey + "=")
	if err != nil || len(key) != 32 {
		return nil, errors.New("invalid wecom encoding aes key")
	}
	return &wecomCipher{key: key}, nil
}

func wecomSignature(token, timestamp, nonce, encrypted string) string {
	parts := []string{token, timestamp, nonce, encrypted}
	sort.Strings(parts)
	sum := sha1.Sum([]byte(strings.Join(parts, "")))
	return hex.EncodeToString(sum[:])
}

func wecomConstantTimeEqual(a, b string) bool {
	return hmac.Equal([]byte(a), []byte(b))
}

func (c *wecomCipher) decrypt(ciphertext []byte) (msg, receiveID []byte, err error) {
	block, err := aes.NewCipher(c.key)
	if err != nil {
		return nil, nil, errors.New("invalid wecom cipher key")
	}
	if len(ciphertext) < aes.BlockSize || len(ciphertext)%aes.BlockSize != 0 {
		return nil, nil, errors.New("invalid wecom ciphertext size")
	}
	plaintext := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, c.key[:aes.BlockSize]).CryptBlocks(plaintext, ciphertext)
	plaintext, err = wecomPKCS7Unpad(plaintext)
	if err != nil {
		return nil, nil, err
	}
	if len(plaintext) < 20 {
		return nil, nil, errors.New("truncated wecom plaintext")
	}
	msgLen := int(binary.BigEndian.Uint32(plaintext[16:20]))
	if msgLen < 0 || msgLen > len(plaintext)-20 {
		return nil, nil, errors.New("invalid wecom message length")
	}
	return plaintext[20 : 20+msgLen], plaintext[20+msgLen:], nil
}

func (c *wecomCipher) encrypt(msg, receiveID []byte) ([]byte, error) {
	block, err := aes.NewCipher(c.key)
	if err != nil {
		return nil, errors.New("invalid wecom cipher key")
	}
	random := make([]byte, 16)
	plaintext := make([]byte, 0, 20+len(msg)+len(receiveID)+aes.BlockSize)
	plaintext = append(plaintext, random...)
	length := make([]byte, 4)
	binary.BigEndian.PutUint32(length, uint32(len(msg)))
	plaintext = append(plaintext, length...)
	plaintext = append(plaintext, msg...)
	plaintext = append(plaintext, receiveID...)
	padded := wecomPKCS7Pad(plaintext, aes.BlockSize)
	ciphertext := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, c.key[:aes.BlockSize]).CryptBlocks(ciphertext, padded)
	return ciphertext, nil
}

func wecomPKCS7Pad(data []byte, blockSize int) []byte {
	padding := blockSize - len(data)%blockSize
	return append(data, bytes.Repeat([]byte{byte(padding)}, padding)...)
}

func wecomPKCS7Unpad(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, errors.New("empty wecom plaintext")
	}
	padding := int(data[len(data)-1])
	if padding == 0 || padding > len(data) || padding > aes.BlockSize {
		return nil, errors.New("invalid wecom padding")
	}
	for _, b := range data[len(data)-padding:] {
		if int(b) != padding {
			return nil, errors.New("invalid wecom padding")
		}
	}
	return data[:len(data)-padding], nil
}

func (wc *WeCom) verifyTimestamp(timestamp string) error {
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return ErrInvalidSignature
	}
	skew := wc.now().Sub(time.Unix(seconds, 0))
	if skew > wecomMaxClockSkew || -skew > wecomMaxClockSkew {
		return ErrInvalidSignature
	}
	return nil
}

// VerifyURL handles the one-time GET callback verification WeCom performs
// when an operator registers the webhook URL. It re-computes the signature
// over the echostr, decrypts it, checks the embedded receiveID equals the
// configured corpid, and returns the plaintext that must be echoed back.
func (wc *WeCom) VerifyURL(r *http.Request, binding config.ChannelConfig) (string, error) {
	token, err := config.Secret(binding.SigningSecretEnv)
	if err != nil {
		return "", err
	}
	query := r.URL.Query()
	echostr := query.Get("echostr")
	if echostr == "" {
		return "", ErrInvalidSignature
	}
	if err := wc.verifyTimestamp(query.Get("timestamp")); err != nil {
		return "", err
	}
	signature := wecomSignature(token, query.Get("timestamp"), query.Get("nonce"), echostr)
	if !wecomConstantTimeEqual(signature, query.Get("msg_signature")) {
		return "", ErrInvalidSignature
	}
	cipherInstance, err := newWeComCipherFromBinding(binding)
	if err != nil {
		return "", err
	}
	ciphertext, err := base64.StdEncoding.DecodeString(echostr)
	if err != nil {
		return "", ErrInvalidSignature
	}
	msg, receiveID, err := cipherInstance.decrypt(ciphertext)
	if err != nil {
		return "", ErrInvalidSignature
	}
	if binding.WorkspaceID != "" && string(receiveID) != binding.WorkspaceID {
		return "", ErrBindingMismatch
	}
	return string(msg), nil
}

// Verify authenticates a POST callback: signature over (token, timestamp,
// nonce, Encrypt) plus a bounded clock skew window, mirroring the Slack
// replay protection.
func (wc *WeCom) Verify(r *http.Request, body []byte, binding config.ChannelConfig) error {
	token, err := config.Secret(binding.SigningSecretEnv)
	if err != nil {
		return err
	}
	query := r.URL.Query()
	if err := wc.verifyTimestamp(query.Get("timestamp")); err != nil {
		return err
	}
	encrypted, err := wecomEncryptField(body)
	if err != nil {
		return ErrInvalidSignature
	}
	signature := wecomSignature(token, query.Get("timestamp"), query.Get("nonce"), encrypted)
	if !wecomConstantTimeEqual(signature, query.Get("msg_signature")) {
		return ErrInvalidSignature
	}
	return nil
}

func wecomEncryptField(body []byte) (string, error) {
	var envelope struct {
		Encrypt string `xml:"Encrypt"`
	}
	if err := xml.Unmarshal(body, &envelope); err != nil || envelope.Encrypt == "" {
		return "", errors.New("missing wecom Encrypt field")
	}
	return envelope.Encrypt, nil
}

type wecomMessage struct {
	ToUserName   string `xml:"ToUserName"`
	FromUserName string `xml:"FromUserName"`
	CreateTime   int64  `xml:"CreateTime"`
	MsgType      string `xml:"MsgType"`
	Content      string `xml:"Content"`
	MsgID        int64  `xml:"MsgId"`
	AgentID      int64  `xml:"AgentID"`
	ChatID       string `xml:"ChatId"`
	Image        *struct {
		MediaID string `xml:"MediaId"`
	} `xml:"Image"`
	File *struct {
		MediaID  string `xml:"MediaId"`
		FileName string `xml:"FileName"`
	} `xml:"File"`
	Event *struct {
		EventType string `xml:"Event"`
	} `xml:"Event"`
}

func (wc *WeCom) Parse(body []byte, binding config.ChannelConfig) (ParsedWebhook, error) {
	encrypted, err := wecomEncryptField(body)
	if err != nil {
		return ParsedWebhook{}, ErrUnsupportedEvent
	}
	cipherInstance, err := newWeComCipherFromBinding(binding)
	if err != nil {
		return ParsedWebhook{}, err
	}
	ciphertext, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil {
		return ParsedWebhook{}, ErrInvalidSignature
	}
	plaintext, receiveID, err := cipherInstance.decrypt(ciphertext)
	if err != nil {
		return ParsedWebhook{}, ErrInvalidSignature
	}
	// receiveID is the corpid; ToUserName repeats it inside the plaintext.
	if binding.WorkspaceID != "" && string(receiveID) != binding.WorkspaceID {
		return ParsedWebhook{}, ErrBindingMismatch
	}
	var m wecomMessage
	if err := xml.Unmarshal(plaintext, &m); err != nil {
		return ParsedWebhook{}, errors.New("decode wecom message: malformed plaintext")
	}
	if m.ToUserName != "" && binding.WorkspaceID != "" && m.ToUserName != binding.WorkspaceID {
		return ParsedWebhook{}, ErrBindingMismatch
	}
	// AgentID binding prevents cross-application routing when multiple apps
	// share one corpid callback domain, mirroring the Slack app binding.
	if m.AgentID != 0 && binding.ApplicationID != "" &&
		strconv.FormatInt(m.AgentID, 10) != binding.ApplicationID {
		return ParsedWebhook{}, ErrBindingMismatch
	}
	if m.MsgType == "event" || m.FromUserName == "" {
		return ParsedWebhook{}, ErrUnsupportedEvent
	}

	attachments := make([]domain.Attachment, 0, 2)
	if m.Image != nil && m.Image.MediaID != "" {
		attachments = append(attachments, domain.Attachment{Type: "image", FileID: m.Image.MediaID})
	}
	if m.File != nil && m.File.MediaID != "" {
		attachments = append(attachments, domain.Attachment{
			Type: "file", FileID: m.File.MediaID, Name: m.File.FileName,
		})
	}
	if strings.TrimSpace(m.Content) == "" && len(attachments) == 0 {
		return ParsedWebhook{}, ErrUnsupportedEvent
	}

	scope := domain.ScopeDirect
	replyTarget := m.FromUserName
	if m.ChatID != "" {
		scope = domain.ScopeGroup
		replyTarget = m.ChatID
	}
	messageID := strconv.FormatInt(m.MsgID, 10)
	if m.MsgID == 0 {
		messageID = fmt.Sprintf("%s:%d", replyTarget, m.CreateTime)
	}
	received := time.Now().UTC()
	if m.CreateTime != 0 {
		received = time.Unix(m.CreateTime, 0).UTC()
	}
	return ParsedWebhook{Messages: []domain.InboundMessage{{
		BindingID: binding.BindingID, Channel: "wecom", ExternalMessageID: messageID,
		ExternalUserID: m.FromUserName, ConversationID: replyTarget,
		Scope: scope, Text: m.Content, Attachments: attachments, ReceivedAt: received,
		ReplyTarget: replyTarget,
	}}}, nil
}

func newWeComCipherFromBinding(binding config.ChannelConfig) (*wecomCipher, error) {
	key, err := config.Secret(binding.EncryptionKeyEnv)
	if err != nil {
		return nil, err
	}
	return newWeComCipher(key)
}

type wecomTokenResponse struct {
	ErrCode     int    `json:"errcode"`
	ErrMsg      string `json:"errmsg"`
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

type wecomSendResponse struct {
	ErrCode int    `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
}

// accessToken fetches and caches the app access token per (corpid, secret).
// The token appears only in request URLs, so every error path returns a
// category instead of the underlying URL-bearing failure.
func (wc *WeCom) accessToken(ctx context.Context, binding config.ChannelConfig) (string, error) {
	secret, err := config.Secret(binding.TokenEnv)
	if err != nil {
		return "", err
	}
	cacheKey := binding.WorkspaceID + "\x1f" + binding.TokenEnv
	wc.mu.Lock()
	cached, ok := wc.tokens[cacheKey]
	if ok && wc.now().Before(cached.expiresAt) {
		token := cached.accessToken
		wc.mu.Unlock()
		return token, nil
	}
	wc.mu.Unlock()

	base := strings.TrimRight(binding.APIBaseURL, "/")
	if base == "" {
		base = wecomDefaultAPIBase
	}
	endpoint := base + "/cgi-bin/gettoken?" + url.Values{
		"corpid":     {binding.WorkspaceID},
		"corpsecret": {secret},
	}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", errors.New("create wecom token request failed")
	}
	resp, err := wc.client.Do(req)
	if err != nil {
		return "", errors.New("wecom token request failed")
	}
	defer resp.Body.Close()
	limited := io.LimitReader(resp.Body, 1<<20)
	var tokenResp wecomTokenResponse
	if err := json.NewDecoder(limited).Decode(&tokenResp); err != nil {
		return "", errors.New("decode wecom token response failed")
	}
	if tokenResp.ErrCode != 0 || tokenResp.AccessToken == "" {
		return "", fmt.Errorf("wecom token rejected: errcode %d", tokenResp.ErrCode)
	}
	expiresIn := time.Duration(tokenResp.ExpiresIn) * time.Second
	if expiresIn <= 0 {
		expiresIn = 7200 * time.Second
	}
	// Refresh five minutes before the real expiry to avoid borderline 42001.
	usable := expiresIn - 5*time.Minute
	if usable <= 0 {
		usable = expiresIn / 2
	}
	wc.mu.Lock()
	wc.tokens[cacheKey] = wecomTokenEntry{
		accessToken: tokenResp.AccessToken,
		expiresAt:   wc.now().Add(usable),
	}
	wc.mu.Unlock()
	return tokenResp.AccessToken, nil
}

func (wc *WeCom) invalidateToken(binding config.ChannelConfig) {
	cacheKey := binding.WorkspaceID + "\x1f" + binding.TokenEnv
	wc.mu.Lock()
	delete(wc.tokens, cacheKey)
	wc.mu.Unlock()
}

func (wc *WeCom) sendOnce(ctx context.Context, binding config.ChannelConfig, token, path string, payload map[string]any) error {
	base := strings.TrimRight(binding.APIBaseURL, "/")
	if base == "" {
		base = wecomDefaultAPIBase
	}
	endpoint := base + path + "?access_token=" + url.QueryEscape(token)
	var response wecomSendResponse
	if err := postJSON(ctx, wc.client, endpoint, nil, payload, &response); err != nil {
		return err
	}
	if response.ErrCode == 40014 || response.ErrCode == 42001 {
		return fmt.Errorf("wecom delivery token expired: errcode %d", response.ErrCode)
	}
	if response.ErrCode != 0 {
		return fmt.Errorf("wecom delivery rejected: errcode %d %s", response.ErrCode, response.ErrMsg)
	}
	return nil
}

func (wc *WeCom) Deliver(ctx context.Context, binding config.ChannelConfig, msg domain.OutboundMessage) error {
	agentID := int64(0)
	if binding.ApplicationID != "" {
		parsed, err := strconv.ParseInt(binding.ApplicationID, 10, 64)
		if err != nil {
			return errors.New("invalid wecom agentid configuration")
		}
		agentID = parsed
	}
	// Group chats use appchat/send keyed by chatid; direct messages use
	// message/send keyed by touser plus agentid.
	path := "/cgi-bin/message/send"
	if msg.Scope == domain.ScopeGroup {
		path = "/cgi-bin/appchat/send"
	}
	for _, part := range chunksUTF8Bytes(msg.Text, binding.MaxMessageLength) {
		payload := map[string]any{
			"msgtype":                  "text",
			"text":                     map[string]any{"content": part},
			"duplicate_check_interval": 1800,
		}
		if msg.Scope == domain.ScopeGroup {
			payload["chatid"] = msg.Target
		} else {
			payload["touser"] = msg.Target
			payload["agentid"] = agentID
		}
		token, err := wc.accessToken(ctx, binding)
		if err != nil {
			return err
		}
		if err := wc.sendOnce(ctx, binding, token, path, payload); err != nil {
			if strings.Contains(err.Error(), "token expired") {
				// Token rotated or expired early: refresh once and retry the part.
				wc.invalidateToken(binding)
				token, err = wc.accessToken(ctx, binding)
				if err != nil {
					return err
				}
				if err := wc.sendOnce(ctx, binding, token, path, payload); err != nil {
					return err
				}
				continue
			}
			return err
		}
	}
	return nil
}

// chunksUTF8Bytes splits text into parts of at most limit bytes without
// cutting a multi-byte rune; WeCom's content limit is expressed in bytes.
func chunksUTF8Bytes(text string, limit int) []string {
	if limit <= 0 {
		limit = wecomDefaultMessageBytes
	}
	if len(text) <= limit {
		if len(text) == 0 {
			return []string{""}
		}
		return []string{text}
	}
	result := make([]string, 0, (len(text)+limit-1)/limit)
	for len(text) > 0 {
		n := limit
		if n > len(text) {
			n = len(text)
		}
		for n < len(text) && !utf8.RuneStart(text[n]) {
			n--
		}
		if n == 0 {
			// A single rune wider than the limit cannot be split safely.
			n = limit
		}
		result = append(result, text[:n])
		text = text[n:]
	}
	return result
}
