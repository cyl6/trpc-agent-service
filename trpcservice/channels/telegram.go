package channels

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cyl6/trpc-agent-service/trpcservice/config"
	"github.com/cyl6/trpc-agent-service/trpcservice/domain"
)

const telegramSecretHeader = "X-Telegram-Bot-Api-Secret-Token"

type Telegram struct {
	client HTTPDoer
}

func NewTelegram(client HTTPDoer) *Telegram {
	if client == nil {
		client = http.DefaultClient
	}
	return &Telegram{client: client}
}

func (*Telegram) Name() string { return "telegram" }

func (*Telegram) Verify(r *http.Request, _ []byte, binding config.ChannelConfig) error {
	secret, err := config.Secret(binding.SigningSecretEnv)
	if err != nil {
		return err
	}
	got := r.Header.Get(telegramSecretHeader)
	if len(got) != len(secret) || subtle.ConstantTimeCompare([]byte(got), []byte(secret)) != 1 {
		return ErrInvalidSignature
	}
	return nil
}

type telegramUpdate struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		MessageID       int64  `json:"message_id"`
		MessageThreadID int64  `json:"message_thread_id"`
		Date            int64  `json:"date"`
		Text            string `json:"text"`
		Caption         string `json:"caption"`
		From            struct {
			ID int64 `json:"id"`
		} `json:"from"`
		Chat struct {
			ID   int64  `json:"id"`
			Type string `json:"type"`
		} `json:"chat"`
		Photo []struct {
			FileID string `json:"file_id"`
		} `json:"photo"`
		Document *struct {
			FileID   string `json:"file_id"`
			FileName string `json:"file_name"`
			MimeType string `json:"mime_type"`
		} `json:"document"`
	} `json:"message"`
}

func (*Telegram) Parse(body []byte, binding config.ChannelConfig) (ParsedWebhook, error) {
	var update telegramUpdate
	if err := json.Unmarshal(body, &update); err != nil {
		return ParsedWebhook{}, fmt.Errorf("decode telegram update: %w", err)
	}
	if update.Message == nil || update.Message.From.ID == 0 || update.Message.Chat.ID == 0 {
		return ParsedWebhook{}, ErrUnsupportedEvent
	}
	m := update.Message
	text := m.Text
	if text == "" {
		text = m.Caption
	}
	attachments := make([]domain.Attachment, 0, len(m.Photo)+1)
	if len(m.Photo) > 0 {
		best := m.Photo[len(m.Photo)-1]
		attachments = append(attachments, domain.Attachment{Type: "image", FileID: best.FileID})
	}
	if m.Document != nil {
		attachments = append(attachments, domain.Attachment{
			Type: "file", FileID: m.Document.FileID, Name: m.Document.FileName, MimeType: m.Document.MimeType,
		})
	}
	if strings.TrimSpace(text) == "" && len(attachments) == 0 {
		return ParsedWebhook{}, ErrUnsupportedEvent
	}
	scope := domain.ScopeGroup
	if m.Chat.Type == "private" {
		scope = domain.ScopeDirect
	}
	received := time.Unix(m.Date, 0).UTC()
	if m.Date == 0 {
		received = time.Now().UTC()
	}
	messageID := strconv.FormatInt(update.UpdateID, 10)
	if update.UpdateID == 0 {
		messageID = fmt.Sprintf("%d:%d", m.Chat.ID, m.MessageID)
	}
	threadID := ""
	if m.MessageThreadID != 0 {
		threadID = strconv.FormatInt(m.MessageThreadID, 10)
	}
	return ParsedWebhook{Messages: []domain.InboundMessage{{
		BindingID: binding.BindingID, Channel: "telegram", ExternalMessageID: messageID,
		ExternalUserID: strconv.FormatInt(m.From.ID, 10), ConversationID: strconv.FormatInt(m.Chat.ID, 10), ThreadID: threadID,
		Scope: scope, Text: text, Attachments: attachments, ReceivedAt: received,
		ReplyTarget: strconv.FormatInt(m.Chat.ID, 10),
	}}}, nil
}

func (t *Telegram) Deliver(ctx context.Context, binding config.ChannelConfig, msg domain.OutboundMessage) error {
	token, err := config.Secret(binding.TokenEnv)
	if err != nil {
		return err
	}
	base := strings.TrimRight(binding.APIBaseURL, "/")
	if base == "" {
		base = "https://api.telegram.org"
	}
	// The token is used only in the request URL and is never included in an error.
	url := base + "/bot" + token + "/sendMessage"
	for _, part := range chunks(msg.Text, binding.MaxMessageLength) {
		var response struct {
			OK          bool   `json:"ok"`
			Description string `json:"description"`
		}
		payload := map[string]any{"chat_id": msg.Target, "text": part}
		if threadID, parseErr := strconv.ParseInt(msg.ThreadID, 10, 64); parseErr == nil && threadID != 0 {
			payload["message_thread_id"] = threadID
		}
		if err := postJSON(ctx, t.client, url, nil, payload, &response); err != nil {
			return err
		}
		if !response.OK {
			return fmt.Errorf("telegram delivery rejected: %s", response.Description)
		}
	}
	return nil
}
