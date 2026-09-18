# External Plugins

External plugins allow you to extend whatsrook with independently developed executable programs. A plugin can be written in any language (Rust, Go, Python, C, etc.), compiled into a standalone binary, installed into whatsrook, and used directly as a WhatsApp command.

External plugins run as isolated child processes managed by the dedicated [`package external`](https://github.com/thruqe/whatsrook/external). They can perform virtually any action an internal plugin can do — sending rich media, polls, reactions, audio voice notes, stickers, documents, and real-time live message edits.

---

## How It Works

1. Create a plugin program in any language.
2. Build an executable for the host operating system and CPU architecture.
3. Install the executable using the `.install` command.
4. Run the plugin by sending its command name in WhatsApp.
5. The plugin reads a rich JSON request from standard input and either:
   - **Simple Mode**: Writes a single text reply to standard output.
   - **Action Protocol Mode**: Streams newline-delimited JSON action frames to standard output to execute rich WhatsApp actions (images, video, audio, stickers, documents, polls, reactions, live in-place edits).

Example:

```text
.install weather
.weather London
```

---

## Plugin Management Commands

| Command                      | Usage                         | Description                                                    |
| ---------------------------- | ----------------------------- | -------------------------------------------------------------- |
| `.install <name>`            | `.install weather`            | Downloads and installs from official registry for host OS/arch |
| `.install all`               | `.install all`                | Installs all 13 official external plugins in parallel          |
| `.install <name> <url/path>` | `.install custom https://...` | Installs from custom URL or local path                         |
| `.plist`                     | `.plist` (or `.pluginlist`)   | Lists all installed external plugins                           |
| `.uninstall <name>`          | `.uninstall weather`          | Uninstalls an external plugin                                  |
| `.uninstall all`             | `.uninstall all`              | Uninstalls all external plugins                                |

---

## Inbound Request Payload (`stdin`)

When a command is triggered, WhatsRook passes a JSON request line on `stdin`:

```json
{
	"command": "calc",
	"args": ["12", "*", "12"],
	"raw_args": "12 * 12",
	"chat": "1234567890@s.whatsapp.net",
	"sender": "9876543210@s.whatsapp.net",
	"prefix": ".",
	"bot_name": "WhatsRook",
	"push_name": "Alice",
	"is_group": true,
	"is_sudo": true,
	"is_owner": true,
	"is_admin": true,
	"live_session": false,
	"is_cancel_request": false,
	"quoted_message": {
		"id": "3EB0ABC12345",
		"sender": "1122334455@s.whatsapp.net",
		"text": "Hello world"
	},
	"mentioned_jids": ["1122334455@s.whatsapp.net"]
}
```

### Request Fields:

| Field            | Type       | Description                                                    |
| ---------------- | ---------- | -------------------------------------------------------------- |
| `command`        | `string`   | The installed plugin command name.                             |
| `args`           | `[]string` | Whitespace-split arguments.                                    |
| `raw_args`       | `string`   | Unparsed argument string following the command.                |
| `chat`           | `string`   | JID of the WhatsApp chat where executed.                       |
| `sender`         | `string`   | JID of the sender.                                             |
| `prefix`         | `string`   | Active command prefix (e.g. `.` or `/`).                       |
| `bot_name`       | `string`   | Configured display name of the bot.                            |
| `push_name`      | `string`   | WhatsApp push display name of the sender.                      |
| `is_group`       | `bool`     | `true` if invoked in a group.                                  |
| `is_sudo`        | `bool`     | `true` if sender is bot owner or in `sudoers`.                 |
| `is_owner`       | `bool`     | `true` if sender is the primary bot owner.                     |
| `is_admin`       | `bool`     | `true` if sender is a group admin.                             |
| `quoted_message` | `object`   | Context of quoted/replied-to message (`id`, `sender`, `text`). |
| `mentioned_jids` | `[]string` | List of mentioned user JIDs.                                   |

---

## Action Frame Protocol (`stdout`)

External plugins can execute any action an internal plugin can do by writing newline-delimited JSON frames to `stdout`.

### 1. Send Text Reply

```json
{ "action": "reply", "text": "Hello world" }
```

WhatsRook responds on `stdin` with an Acknowledgment frame:

```json
{ "ok": true, "msg_id": "3EB0ABC12345" }
```

### 2. Live In-Place Edit

```json
{ "action": "edit", "msg_id": "3EB0ABC12345", "text": "Updated text content" }
```

### 3. Emoji Reaction

```json
{ "action": "react", "emoji": "🔥" }
```

### 4. Delete / Revoke Message

```json
{ "action": "delete", "msg_id": "3EB0ABC12345" }
```

### 5. Send Image

```json
{
	"action": "send_image",
	"data": "https://example.com/chart.png",
	"caption": "Market Chart"
}
```

_(Supports HTTP/HTTPS URLs or base64 data strings)._

### 6. Send Audio / Voice Note (PTT)

```json
{ "action": "send_audio", "data": "https://example.com/audio.ogg", "ptt": true }
```

### 7. Send Video / GIF

```json
{
	"action": "send_video",
	"data": "https://example.com/animation.mp4",
	"caption": "Check this out",
	"gif_playback": true
}
```

### 8. Send Document

```json
{
	"action": "send_document",
	"data": "https://example.com/report.pdf",
	"filename": "report.pdf",
	"caption": "Annual Report"
}
```

### 9. Send Sticker

```json
{ "action": "send_sticker", "data": "https://example.com/sticker.webp" }
```

### 10. Send Interactive Poll

```json
{
	"action": "poll",
	"question": "What is your favorite crypto?",
	"options": ["Bitcoin", "Ethereum", "Solana"],
	"selectable": 1
}
```

### 11. Typing Loader Indicator

```json
{ "action": "loader", "text": "Processing your request..." }
```

### 12. Conclude Session

```json
{ "action": "done" }
```

---

## Rust SDK Example (`whatsrook-sdk`)

```rust
use whatsrook_sdk::{
    create_http_client, respond, send_action, send_done, send_edit_live, send_image,
    send_poll, send_react, send_reply_live, Action, Request,
};

fn main() {
    let req = Request::load();

    // Check permissions
    if req.is_group() && !req.is_admin() {
        send_react("❌");
        respond("This feature is for group admins only.");
        return;
    }

    // Access quoted text if user replied to another message
    if let Some(quoted) = req.quoted_text() {
        println!("User replied to: {}", quoted);
    }

    // React to the message
    send_react("🚀");

    // Send an interactive poll
    send_poll("Which asset to track?", &["BTC", "ETH", "Gold"]);

    // Send a live ticker message with in-place edits
    if let Some(msg_id) = send_reply_live("⏳ Initializing live tracker...") {
        for i in 1..=5 {
            std::thread::sleep(std::time::Duration::from_millis(1500));
            send_edit_live(&msg_id, &format!("📈 Tracker tick #{}...", i));
        }
    }

    send_done();
}
```

---

## Supported Architectures & WebAssembly (WASM)

WhatsRook supports two execution engines for external plugins:

### 1. WebAssembly / WASI Plugins (`.wasm`)

WebAssembly modules run inside an embedded, pure-Go sandboxed runtime ([`wazero`](https://github.com/tetratelabs/wazero)). A single `.wasm` module runs universally on all operating systems and architectures with sub-millisecond startup times and memory isolation.

```text
# Install any WebAssembly plugin
.install calc https://example.com/calc.wasm
.calc 2 + 2
```

To build a Rust plugin as WASM:

```bash
cargo build --target wasm32-wasip1 --release
```

### 2. Native Executables

- **Linux AMD64 (Static MUSL)**: `x86_64-unknown-linux-musl`
- **Linux ARM64 / Android Termux (Static MUSL)**: `aarch64-unknown-linux-musl`
- **macOS Apple Silicon**: `aarch64-apple-darwin`
- **macOS Intel**: `x86_64-apple-darwin`
- **Windows x64**: `x86_64-pc-windows-msvc`
