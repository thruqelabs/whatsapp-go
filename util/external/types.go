package external

import (
	"time"
)

const (
	// DefaultPluginDirEnv is the environment variable override for external plugins directory.
	DefaultPluginDirEnv = "WHATSROOK_PLUGIN_DIR"
	// DefaultReleaseRegistry is the official release registry URL.
	DefaultReleaseRegistry = "https://github.com/thruqe/whatsrook-externals/releases/latest/download"
	// MaxPluginBinarySize is the maximum allowed size for an external plugin binary (64 MiB).
	MaxPluginBinarySize = 64 << 20
	// DefaultPluginTimeout is the standard execution timeout for one-shot plugins.
	DefaultPluginTimeout = 30 * time.Second
	// DefaultLivePluginTimeout is the timeout for streaming/long-running plugins.
	DefaultLivePluginTimeout = 5 * time.Minute
)

// OfficialPlugins is the list of official external plugins maintained for WhatsRook.
var OfficialPlugins = []string{
	"weather", "urban", "shorturl", "calc", "fact",
	"quotes", "joke", "rizz", "btc", "markets",
	"news", "wabeta", "why", "ss", "tts",
	"qrcode", "fancy", "font", "fonts", "git", "mp4url", "cpu",
	"memory", "captcha", "sticker", "media",
}

// Manifest represents the metadata stored alongside an installed external plugin executable (<name>.json).
type Manifest struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Category    string `json:"category,omitempty"`
	Version     string `json:"version,omitempty"`
	Author      string `json:"author,omitempty"`
	IsPublic    bool   `json:"is_public,omitempty"`
}

// PluginInfo describes an installed plugin for listing purposes.
type PluginInfo struct {
	Name        string    `json:"name"`
	Path        string    `json:"path"`
	Description string    `json:"description,omitempty"`
	IsPublic    bool      `json:"is_public"`
	Size        int64     `json:"size"`
	ModTime     time.Time `json:"mod_time"`
}

// QuotedMessagePayload captures quoted message context for external plugins.
type QuotedMessagePayload struct {
	ID     string `json:"id,omitempty"`
	Sender string `json:"sender,omitempty"`
	Text   string `json:"text,omitempty"`
}

// MediaPayload represents media attached to or quoted in the message.
type MediaPayload struct {
	Path     string `json:"path,omitempty"`      // local filesystem path to the downloaded media file
	MimeType string `json:"mimetype,omitempty"`  // media MIME type (e.g. image/jpeg, video/mp4, image/webp)
	IsQuoted bool   `json:"is_quoted,omitempty"` // true if media was extracted from a quoted message
}

// Request is the rich context payload sent to an external plugin via standard input.
type Request struct {
	Command         string                `json:"command"`
	Args            []string              `json:"args,omitempty"`
	RawArgs         string                `json:"raw_args,omitempty"`
	Chat            string                `json:"chat"`
	Sender          string                `json:"sender"`
	Prefix          string                `json:"prefix"`
	BotName         string                `json:"bot_name"`
	PushName        string                `json:"push_name,omitempty"`
	IsGroup         bool                  `json:"is_group"`
	IsSudo          bool                  `json:"is_sudo"`
	IsOwner         bool                  `json:"is_owner"`
	IsAdmin         bool                  `json:"is_admin,omitempty"`
	IsLiveSession   bool                  `json:"live_session,omitempty"`
	IsCancelRequest bool                  `json:"is_cancel_request,omitempty"`
	QuotedMessage   *QuotedMessagePayload `json:"quoted_message,omitempty"`
	MentionedJIDs   []string              `json:"mentioned_jids,omitempty"`
	Media           *MediaPayload         `json:"media,omitempty"`
	StickerPack     string                `json:"sticker_pack,omitempty"`
	StickerAuthor   string                `json:"sticker_author,omitempty"`
}

// Action represents an action frame sent by the external plugin to WhatsRook via standard output.
type Action struct {
	Action      string   `json:"action"`                 // "reply" | "edit" | "react" | "delete" | "send_image" | "send_audio" | "send_video" | "send_document" | "send_sticker" | "poll" | "loader" | "done"
	Text        string   `json:"text,omitempty"`         // reply, edit, loader text
	MsgID       string   `json:"msg_id,omitempty"`       // edit, react, delete target ID
	Emoji       string   `json:"emoji,omitempty"`        // react emoji
	Data        string   `json:"data,omitempty"`         // base64 encoded media or URL
	MimeType    string   `json:"mimetype,omitempty"`     // media mime type (e.g. "image/png", "audio/ogg")
	Caption     string   `json:"caption,omitempty"`      // image, video, document caption
	Filename    string   `json:"filename,omitempty"`     // document filename
	Ptt         bool     `json:"ptt,omitempty"`          // voice note flag for audio
	GifPlayback bool     `json:"gif_playback,omitempty"` // GIF playback for video
	Question    string   `json:"question,omitempty"`     // poll question
	Options     []string `json:"options,omitempty"`      // poll options
	Selectable  int      `json:"selectable,omitempty"`   // poll max selectable count
	Mentions    []string `json:"mentions,omitempty"`     // user JIDs to mention
}

// Ack is sent by WhatsRook to the external plugin via standard input following actions that return results (e.g. "reply", "send_image").
type Ack struct {
	OK    bool   `json:"ok"`
	MsgID string `json:"msg_id,omitempty"`
	Error string `json:"error,omitempty"`
}
