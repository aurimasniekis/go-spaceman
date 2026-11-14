package botapi

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// GetMe returns the bot's own user (API#getme).
func (c *Client) GetMe(ctx context.Context) (User, error) {
	var u User
	err := c.Call(ctx, "getMe", nil, &u)
	return u, err
}

// SetWebhookParams are setWebhook's parameters (API#setwebhook). SecretToken is 1–256 characters of [A-Za-z0-9_-].
type SetWebhookParams struct {
	URL                string   `json:"url"`
	SecretToken        string   `json:"secret_token,omitempty"`
	MaxConnections     int      `json:"max_connections,omitempty"`
	AllowedUpdates     []string `json:"allowed_updates,omitempty"`
	DropPendingUpdates bool     `json:"drop_pending_updates,omitempty"`
	IPAddress          string   `json:"ip_address,omitempty"`
}

// SetWebhook registers the webhook. The secret token is redacted from the error like the bot token.
func (c *Client) SetWebhook(ctx context.Context, p SetWebhookParams) error {
	err := c.Call(ctx, "setWebhook", p, nil)
	var e *Error
	if p.SecretToken != "" && errors.As(err, &e) {
		e.Description = redactString(e.Description, p.SecretToken)
	}
	return err
}

// DeleteWebhook removes the webhook (API#deletewebhook).
func (c *Client) DeleteWebhook(ctx context.Context, dropPendingUpdates bool) error {
	return c.Call(ctx, "deleteWebhook", map[string]bool{"drop_pending_updates": dropPendingUpdates}, nil)
}

// GetWebhookInfo returns the webhook status (API#getwebhookinfo).
func (c *Client) GetWebhookInfo(ctx context.Context) (WebhookInfo, error) {
	var w WebhookInfo
	err := c.Call(ctx, "getWebhookInfo", nil, &w)
	return w, err
}

// GetUpdatesParams are getUpdates' parameters (API#getupdates). Limit is 1–100; Timeout is the long-poll wait in
// seconds, so the call's context must allow longer than that.
type GetUpdatesParams struct {
	Offset         int64    `json:"offset,omitempty"`
	Limit          int      `json:"limit,omitempty"`
	Timeout        int      `json:"timeout,omitempty"`
	AllowedUpdates []string `json:"allowed_updates,omitempty"`
}

// GetUpdates long-polls for updates; an update is confirmed by a later call with a higher Offset.
func (c *Client) GetUpdates(ctx context.Context, p GetUpdatesParams) ([]Update, error) {
	var u []Update
	err := c.Call(ctx, "getUpdates", p, &u)
	return u, err
}

// SendMessageParams are sendMessage's parameters (API#sendmessage). Text is 1–4096 characters; SpaceMan formats with
// Entities, never parse_mode.
type SendMessageParams struct {
	ChatID              int64               `json:"chat_id"`
	MessageThreadID     int                 `json:"message_thread_id,omitempty"`
	Text                string              `json:"text"`
	Entities            []MessageEntity     `json:"entities,omitempty"`
	LinkPreviewOptions  *LinkPreviewOptions `json:"link_preview_options,omitempty"`
	DisableNotification bool                `json:"disable_notification,omitempty"`
	ProtectContent      bool                `json:"protect_content,omitempty"`
	ReplyParameters     *ReplyParameters    `json:"reply_parameters,omitempty"`
	ReplyMarkup         ReplyMarkup         `json:"reply_markup,omitempty"`
}

// SendMessage sends a text message.
func (c *Client) SendMessage(ctx context.Context, p SendMessageParams) (Message, error) {
	var m Message
	err := c.Call(ctx, "sendMessage", p, &m)
	return m, err
}

// EditMessageTextParams are editMessageText's parameters for a chat message (API#editmessagetext).
type EditMessageTextParams struct {
	ChatID             int64                 `json:"chat_id"`
	MessageID          int                   `json:"message_id"`
	Text               string                `json:"text"`
	Entities           []MessageEntity       `json:"entities,omitempty"`
	LinkPreviewOptions *LinkPreviewOptions   `json:"link_preview_options,omitempty"`
	ReplyMarkup        *InlineKeyboardMarkup `json:"reply_markup,omitempty"`
}

// EditMessageText replaces a message's text and keyboard.
func (c *Client) EditMessageText(ctx context.Context, p EditMessageTextParams) (Message, error) {
	var m Message
	err := c.Call(ctx, "editMessageText", p, &m)
	return m, err
}

// EditMessageReplyMarkupParams are editMessageReplyMarkup's parameters (API#editmessagereplymarkup). A nil
// ReplyMarkup removes the keyboard.
type EditMessageReplyMarkupParams struct {
	ChatID      int64                 `json:"chat_id"`
	MessageID   int                   `json:"message_id"`
	ReplyMarkup *InlineKeyboardMarkup `json:"reply_markup,omitempty"`
}

// EditMessageReplyMarkup replaces only a message's keyboard.
func (c *Client) EditMessageReplyMarkup(ctx context.Context, p EditMessageReplyMarkupParams) (Message, error) {
	var m Message
	err := c.Call(ctx, "editMessageReplyMarkup", p, &m)
	return m, err
}

// DeleteMessage deletes a message (within 48 h, API#deletemessage).
func (c *Client) DeleteMessage(ctx context.Context, chatID int64, messageID int) error {
	return c.Call(ctx, "deleteMessage", map[string]int64{"chat_id": chatID, "message_id": int64(messageID)}, nil)
}

// AnswerCallbackQueryParams are answerCallbackQuery's parameters (API#answercallbackquery). Text is 0–200 characters.
type AnswerCallbackQueryParams struct {
	CallbackQueryID string `json:"callback_query_id"`
	Text            string `json:"text,omitempty"`
	ShowAlert       bool   `json:"show_alert,omitempty"`
	URL             string `json:"url,omitempty"`
	CacheTime       int    `json:"cache_time,omitempty"`
}

// AnswerCallbackQuery stops the client's spinner for a callback query; it must always be called.
func (c *Client) AnswerCallbackQuery(ctx context.Context, p AnswerCallbackQueryParams) error {
	return c.Call(ctx, "answerCallbackQuery", p, nil)
}

// SetMyCommandsParams are setMyCommands' parameters (API#setmycommands): at most 100 commands.
type SetMyCommandsParams struct {
	Commands     []BotCommand     `json:"commands"`
	Scope        *BotCommandScope `json:"scope,omitempty"`
	LanguageCode string           `json:"language_code,omitempty"`
}

// SetMyCommands sets the command menu.
func (c *Client) SetMyCommands(ctx context.Context, p SetMyCommandsParams) error {
	return c.Call(ctx, "setMyCommands", p, nil)
}

// SetChatMenuButtonParams are setChatMenuButton's parameters (API#setchatmenubutton); ChatID 0 sets the default.
type SetChatMenuButtonParams struct {
	ChatID     int64       `json:"chat_id,omitempty"`
	MenuButton *MenuButton `json:"menu_button,omitempty"`
}

// SetChatMenuButton sets the menu button.
func (c *Client) SetChatMenuButton(ctx context.Context, p SetChatMenuButtonParams) error {
	return c.Call(ctx, "setChatMenuButton", p, nil)
}

// GetFile prepares a file for DownloadFile (API#getfile); files over 20 MB are refused by Telegram.
func (c *Client) GetFile(ctx context.Context, fileID string) (File, error) {
	var f File
	err := c.Call(ctx, "getFile", map[string]string{"file_id": fileID}, &f)
	return f, err
}

// SendDocumentParams are sendDocument's parameters (API#senddocument). Caption is 0–1024 characters; uploads are up
// to 50 MB.
type SendDocumentParams struct {
	ChatID              int64            `json:"chat_id"`
	MessageThreadID     int              `json:"message_thread_id,omitempty"`
	Document            *InputFile       `json:"document"`
	Caption             string           `json:"caption,omitempty"`
	CaptionEntities     []MessageEntity  `json:"caption_entities,omitempty"`
	DisableNotification bool             `json:"disable_notification,omitempty"`
	ProtectContent      bool             `json:"protect_content,omitempty"`
	ReplyParameters     *ReplyParameters `json:"reply_parameters,omitempty"`
	ReplyMarkup         ReplyMarkup      `json:"reply_markup,omitempty"`
}

// SendDocument sends a file by ID or upload.
func (c *Client) SendDocument(ctx context.Context, p SendDocumentParams) (Message, error) {
	if p.Document == nil {
		return Message{}, errors.New("telegram sendDocument: no document")
	}
	var m Message
	err := c.callMultipart(ctx, "sendDocument", p, uploadsOf(p.Document), &m)
	return m, err
}

// SendPhotoParams are sendPhoto's parameters (API#sendphoto). Uploads are up to 10 MB.
type SendPhotoParams struct {
	ChatID              int64            `json:"chat_id"`
	MessageThreadID     int              `json:"message_thread_id,omitempty"`
	Photo               *InputFile       `json:"photo"`
	Caption             string           `json:"caption,omitempty"`
	CaptionEntities     []MessageEntity  `json:"caption_entities,omitempty"`
	DisableNotification bool             `json:"disable_notification,omitempty"`
	ProtectContent      bool             `json:"protect_content,omitempty"`
	ReplyParameters     *ReplyParameters `json:"reply_parameters,omitempty"`
	ReplyMarkup         ReplyMarkup      `json:"reply_markup,omitempty"`
}

// SendPhoto sends a photo by ID or upload.
func (c *Client) SendPhoto(ctx context.Context, p SendPhotoParams) (Message, error) {
	if p.Photo == nil {
		return Message{}, errors.New("telegram sendPhoto: no photo")
	}
	var m Message
	err := c.callMultipart(ctx, "sendPhoto", p, uploadsOf(p.Photo), &m)
	return m, err
}

// SendMediaGroupParams are sendMediaGroup's parameters (API#sendmediagroup): 2–10 items; documents group only with
// documents.
type SendMediaGroupParams struct {
	ChatID              int64            `json:"chat_id"`
	MessageThreadID     int              `json:"message_thread_id,omitempty"`
	Media               []InputMedia     `json:"media"`
	DisableNotification bool             `json:"disable_notification,omitempty"`
	ProtectContent      bool             `json:"protect_content,omitempty"`
	ReplyParameters     *ReplyParameters `json:"reply_parameters,omitempty"`
}

// SendMediaGroup sends an album and returns its messages.
func (c *Client) SendMediaGroup(ctx context.Context, p SendMediaGroupParams) ([]Message, error) {
	files := make([]*InputFile, 0, len(p.Media))
	for i, m := range p.Media {
		if m.Media == nil {
			return nil, fmt.Errorf("telegram sendMediaGroup: item %d has no media", i)
		}
		files = append(files, m.Media)
	}
	var ms []Message
	err := c.callMultipart(ctx, "sendMediaGroup", p, uploadsOf(files...), &ms)
	return ms, err
}

func uploadsOf(files ...*InputFile) []*InputFile {
	var up []*InputFile
	for _, f := range files {
		if f.isUpload() {
			up = append(up, f)
		}
	}
	return up
}

func redactString(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "<redacted>")
}
