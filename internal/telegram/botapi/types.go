package botapi

import (
	"encoding/json"
	"io"
)

// The types below carry only the fields SpaceMan reads or sends (API#available-types). Unknown fields in answers are
// ignored, so Telegram adding fields never breaks decoding.

// ResponseParameters helps handle some errors automatically (API#responseparameters).
type ResponseParameters struct {
	MigrateToChatID int64 `json:"migrate_to_chat_id,omitempty"`
	RetryAfter      int   `json:"retry_after,omitempty"`
}

// User is a Telegram user or bot (API#user).
type User struct {
	ID               int64  `json:"id"`
	IsBot            bool   `json:"is_bot"`
	FirstName        string `json:"first_name"`
	LastName         string `json:"last_name,omitempty"`
	Username         string `json:"username,omitempty"`
	LanguageCode     string `json:"language_code,omitempty"`
	HasTopicsEnabled bool   `json:"has_topics_enabled,omitempty"`
}

// Chat types (API#chat).
const (
	ChatPrivate    = "private"
	ChatGroup      = "group"
	ChatSupergroup = "supergroup"
	ChatChannel    = "channel"
)

// Chat is a chat (API#chat).
type Chat struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"`
	Title     string `json:"title,omitempty"`
	Username  string `json:"username,omitempty"`
	FirstName string `json:"first_name,omitempty"`
	IsForum   bool   `json:"is_forum,omitempty"`
}

// MessageEntity is a formatted span; Offset and Length count UTF-16 code units (API#messageentity).
type MessageEntity struct {
	Type          string `json:"type"`
	Offset        int    `json:"offset"`
	Length        int    `json:"length"`
	URL           string `json:"url,omitempty"`
	User          *User  `json:"user,omitempty"`
	Language      string `json:"language,omitempty"`
	CustomEmojiID string `json:"custom_emoji_id,omitempty"`
}

// PhotoSize is one size of a photo (API#photosize).
type PhotoSize struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	FileSize     int64  `json:"file_size,omitempty"`
}

// Document is a general file (API#document).
type Document struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileName     string `json:"file_name,omitempty"`
	MimeType     string `json:"mime_type,omitempty"`
	FileSize     int64  `json:"file_size,omitempty"`
}

// Message is a message (API#message). ForwardOrigin is kept raw: MessageOrigin has several variants and the inbox
// capture (TG-8a) decodes the ones it shows.
type Message struct {
	MessageID       int                   `json:"message_id"`
	MessageThreadID int                   `json:"message_thread_id,omitempty"`
	From            *User                 `json:"from,omitempty"`
	Chat            Chat                  `json:"chat"`
	Date            int64                 `json:"date"`
	EditDate        int64                 `json:"edit_date,omitempty"`
	MediaGroupID    string                `json:"media_group_id,omitempty"`
	ForwardOrigin   json.RawMessage       `json:"forward_origin,omitempty"`
	ReplyToMessage  *Message              `json:"reply_to_message,omitempty"`
	Text            string                `json:"text,omitempty"`
	Entities        []MessageEntity       `json:"entities,omitempty"`
	Caption         string                `json:"caption,omitempty"`
	CaptionEntities []MessageEntity       `json:"caption_entities,omitempty"`
	Photo           []PhotoSize           `json:"photo,omitempty"`
	Document        *Document             `json:"document,omitempty"`
	ReplyMarkup     *InlineKeyboardMarkup `json:"reply_markup,omitempty"`
}

// CallbackQuery is a tap on an inline button (API#callbackquery). Message may be an inaccessible message (date 0).
type CallbackQuery struct {
	ID           string   `json:"id"`
	From         User     `json:"from"`
	Message      *Message `json:"message,omitempty"`
	ChatInstance string   `json:"chat_instance"`
	Data         string   `json:"data,omitempty"`
}

// Update is one incoming update (API#update).
type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message,omitempty"`
	EditedMessage *Message       `json:"edited_message,omitempty"`
	CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
	// MyChatMember reports a change of the bot's own membership in a chat (added to a group, blocked in a private chat).
	MyChatMember *ChatMemberUpdated `json:"my_chat_member,omitempty"`
}

// Chat member statuses (API#chatmember).
const (
	MemberCreator       = "creator"
	MemberAdministrator = "administrator"
	MemberMember        = "member"
	MemberRestricted    = "restricted"
	MemberLeft          = "left"
	MemberKicked        = "kicked"
)

// ChatMember is a member's status in a chat (API#chatmember); only the fields SpaceMan reads.
type ChatMember struct {
	Status string `json:"status"`
	User   User   `json:"user"`
}

// ChatMemberUpdated is a change of a chat member's status (API#chatmemberupdated).
type ChatMemberUpdated struct {
	Chat          Chat       `json:"chat"`
	From          User       `json:"from"`
	Date          int64      `json:"date"`
	OldChatMember ChatMember `json:"old_chat_member"`
	NewChatMember ChatMember `json:"new_chat_member"`
}

// ReplyMarkup is an inline keyboard or a force-reply request.
type ReplyMarkup interface{ replyMarkup() }

// InlineKeyboardMarkup is an inline keyboard (API#inlinekeyboardmarkup).
type InlineKeyboardMarkup struct {
	InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
}

func (*InlineKeyboardMarkup) replyMarkup() {}

// Button styles (Bot API 9.4).
const (
	StyleDanger  = "danger"
	StyleSuccess = "success"
	StylePrimary = "primary"
)

// InlineKeyboardButton is one inline button; exactly one action field is set (API#inlinekeyboardbutton).
// CallbackData is 1–64 bytes; WebApp works only in private chats.
type InlineKeyboardButton struct {
	Text         string          `json:"text"`
	CallbackData string          `json:"callback_data,omitempty"`
	URL          string          `json:"url,omitempty"`
	WebApp       *WebAppInfo     `json:"web_app,omitempty"`
	CopyText     *CopyTextButton `json:"copy_text,omitempty"`
	Style        string          `json:"style,omitempty"`
}

// WebAppInfo names a Mini App URL (API#webappinfo).
type WebAppInfo struct {
	URL string `json:"url"`
}

// CopyTextButton copies text to the clipboard (API#copytextbutton).
type CopyTextButton struct {
	Text string `json:"text"`
}

// ForceReply asks the client to show a reply interface (API#forcereply).
type ForceReply struct {
	ForceReply            bool   `json:"force_reply"`
	InputFieldPlaceholder string `json:"input_field_placeholder,omitempty"`
	Selective             bool   `json:"selective,omitempty"`
}

func (*ForceReply) replyMarkup() {}

// ReplyParameters makes a message a reply (API#replyparameters).
type ReplyParameters struct {
	MessageID int `json:"message_id"`
}

// LinkPreviewOptions controls the link preview (API#linkpreviewoptions).
type LinkPreviewOptions struct {
	IsDisabled bool `json:"is_disabled,omitempty"`
}

// File is a file ready to download with DownloadFile (API#file).
type File struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileSize     int64  `json:"file_size,omitempty"`
	FilePath     string `json:"file_path,omitempty"`
}

// WebhookInfo is the webhook status (API#webhookinfo).
type WebhookInfo struct {
	URL                          string   `json:"url"`
	HasCustomCertificate         bool     `json:"has_custom_certificate"`
	PendingUpdateCount           int      `json:"pending_update_count"`
	IPAddress                    string   `json:"ip_address,omitempty"`
	LastErrorDate                int64    `json:"last_error_date,omitempty"`
	LastErrorMessage             string   `json:"last_error_message,omitempty"`
	LastSynchronizationErrorDate int64    `json:"last_synchronization_error_date,omitempty"`
	MaxConnections               int      `json:"max_connections,omitempty"`
	AllowedUpdates               []string `json:"allowed_updates,omitempty"`
}

// BotCommand is one entry of the command menu (API#botcommand).
type BotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// BotCommandScope limits where commands show (API#botcommandscope): "default", "all_private_chats", "chat", ….
type BotCommandScope struct {
	Type   string `json:"type"`
	ChatID int64  `json:"chat_id,omitempty"`
}

// Menu button types (API#menubutton).
const (
	MenuButtonCommands = "commands"
	MenuButtonWebApp   = "web_app"
	MenuButtonDefault  = "default"
)

// MenuButton is the bot's menu button (API#menubutton); Text and WebApp only for "web_app".
type MenuButton struct {
	Type   string      `json:"type"`
	Text   string      `json:"text,omitempty"`
	WebApp *WebAppInfo `json:"web_app,omitempty"`
}

// InputFile is a file to send: an existing FileID, or an upload read from Reader and named Name (API#inputfile).
type InputFile struct {
	FileID string
	Name   string
	Reader io.Reader

	attach string // multipart field name while a request is being built
}

// MarshalJSON writes the file ID, or the attach:// reference of the upload.
func (f *InputFile) MarshalJSON() ([]byte, error) {
	if f.Reader != nil {
		return json.Marshal("attach://" + f.attach)
	}
	return json.Marshal(f.FileID)
}

func (f *InputFile) isUpload() bool { return f != nil && f.Reader != nil }

// Input media types (API#inputmedia).
const (
	MediaPhoto    = "photo"
	MediaDocument = "document"
)

// InputMedia is one item of a media group: InputMediaPhoto or InputMediaDocument (API#inputmedia).
type InputMedia struct {
	Type            string          `json:"type"`
	Media           *InputFile      `json:"media"`
	Caption         string          `json:"caption,omitempty"`
	CaptionEntities []MessageEntity `json:"caption_entities,omitempty"`
}
