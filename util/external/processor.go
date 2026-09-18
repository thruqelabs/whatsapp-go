package external

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"
	"whatsrook/util/logger"

	"whatsrook"

	"go.mau.fi/whatsmeow/types"
)

// runProcess launches an external plugin executable and handles both streaming action frames and plain text stdout.
func (d *Dispatcher) runProcess(plugCtx *whatsrook.PluginContext, path, name string, request Request) {
	liveCtx, liveCancel := context.WithTimeout(context.Background(), d.liveTimeout)
	defer liveCancel()

	// Update plugCtx context to liveCtx for outbound message sending
	plugCtx.Ctx = liveCtx

	sessionKey := d.sessionKey(request.Chat, name)

	cmd := exec.CommandContext(liveCtx, path)
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		logger.Error("external plugin stdout pipe failed", "plugin", name, "err", err)
		return
	}
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		logger.Error("external plugin stdin pipe failed", "plugin", name, "err", err)
		return
	}

	if err := cmd.Start(); err != nil {
		logger.Error("external plugin start failed", "plugin", name, "err", err)
		_ = plugCtx.Replyf("Failed to start external plugin %q: %v", name, err)
		return
	}

	// Register live session for cancel command handling
	d.registerSession(sessionKey, liveCancel, name)

	defer func() {
		_ = stdinPipe.Close()
		_ = cmd.Wait()
		d.unregisterSession(sessionKey)
	}()

	// Serialize and write initial Request JSON line to stdin
	reqBytes, _ := json.Marshal(request)
	_, _ = fmt.Fprintf(stdinPipe, "%s\n", reqBytes)

	reader := bufio.NewReaderSize(stdoutPipe, 128*1024)
	var firstLine string
	var isStreaming bool
	var readFirst bool

	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			trimmed := strings.TrimSpace(line)
			if trimmed != "" {
				if !readFirst {
					readFirst = true
					firstLine = trimmed
					if strings.HasPrefix(trimmed, "{") && strings.Contains(trimmed, `"action"`) {
						isStreaming = true
					} else {
						// Plain text mode: close stdinPipe immediately so plugin receives EOF on stdin
						isStreaming = false
						_ = stdinPipe.Close()

						var sb strings.Builder
						sb.WriteString(firstLine)
						for {
							restLine, restErr := reader.ReadString('\n')
							if len(restLine) > 0 {
								sb.WriteString("\n")
								sb.WriteString(strings.TrimSpace(restLine))
							}
							if restErr != nil {
								break
							}
						}
						response := strings.TrimSpace(sb.String())
						if response != "" {
							_ = plugCtx.Reply(response)
						}
						break
					}
				}

				if isStreaming {
					if errAction := d.handleActionFrame(plugCtx, stdinPipe, trimmed); errAction != nil {
						logger.Debug("external plugin streaming action finished", "plugin", name, "err", errAction)
						break
					}
				}
			}
		}
		if err != nil {
			if err != io.EOF {
				logger.Warn("external plugin stdout read finished", "plugin", name, "err", err)
			}
			break
		}
	}
}

// handleActionFrame processes a single action frame emitted by an external plugin on stdout.
func (d *Dispatcher) handleActionFrame(ctx *whatsrook.PluginContext, stdinPipe io.WriteCloser, line string) error {
	var frame Action
	if err := json.Unmarshal([]byte(line), &frame); err != nil {
		return err
	}

	switch frame.Action {
	case "reply":
		msgID, err := ctx.ReplyWithID(frame.Text)
		d.sendAck(stdinPipe, err == nil, string(msgID), err)

	case "edit":
		if frame.MsgID != "" && frame.Text != "" {
			_, _ = ctx.Edit(types.MessageID(frame.MsgID), frame.Text)
		}

	case "react":
		if frame.Emoji != "" {
			_ = ctx.React(frame.Emoji)
		}

	case "delete", "revoke":
		if frame.MsgID != "" {
			_, _ = ctx.Delete(types.MessageID(frame.MsgID))
		}

	case "send_image":
		data, err := resolveMediaData(frame.Data)
		if err != nil {
			d.sendAck(stdinPipe, false, "", err)
			return nil
		}
		mime := frame.MimeType
		if mime == "" {
			mime = "image/png"
		}
		err = ctx.ReplyWithImage(data, mime, frame.Caption)
		d.sendAck(stdinPipe, err == nil, "", err)

	case "send_audio":
		data, err := resolveMediaData(frame.Data)
		if err != nil {
			d.sendAck(stdinPipe, false, "", err)
			return nil
		}
		mime := frame.MimeType
		if mime == "" {
			mime = "audio/ogg; codecs=opus"
		}
		err = ctx.ReplyWithAudio(data, mime)
		d.sendAck(stdinPipe, err == nil, "", err)

	case "send_video":
		data, err := resolveMediaData(frame.Data)
		if err != nil {
			d.sendAck(stdinPipe, false, "", err)
			return nil
		}
		mime := frame.MimeType
		if mime == "" {
			mime = "video/mp4"
		}
		if frame.GifPlayback {
			err = ctx.ReplyWithVideoGif(data, mime, frame.Caption)
		} else {
			err = ctx.ReplyWithVideo(data, mime, frame.Caption)
		}
		d.sendAck(stdinPipe, err == nil, "", err)

	case "send_document":
		data, err := resolveMediaData(frame.Data)
		if err != nil {
			d.sendAck(stdinPipe, false, "", err)
			return nil
		}
		filename := frame.Filename
		if filename == "" {
			filename = "document"
		}
		mime := frame.MimeType
		if mime == "" {
			mime = "application/octet-stream"
		}
		err = ctx.ReplyWithDocument(data, mime, filename, frame.Caption)
		d.sendAck(stdinPipe, err == nil, "", err)

	case "send_sticker":
		data, err := resolveMediaData(frame.Data)
		if err != nil {
			logger.Error("send_sticker: resolve media data failed", "err", err)
			d.sendAck(stdinPipe, false, "", err)
			return nil
		}
		logger.Debug("send_sticker: sending sticker to chat", "chat", ctx.Chat.String(), "bytes", len(data))
		err = ctx.ReplyWithSticker(data)
		if err != nil {
			logger.Error("send_sticker: ReplyWithSticker failed", "chat", ctx.Chat.String(), "err", err)
		} else {
			logger.Debug("send_sticker: ReplyWithSticker succeeded", "chat", ctx.Chat.String())
		}
		d.sendAck(stdinPipe, err == nil, "", err)

	case "poll":
		if frame.Question != "" && len(frame.Options) > 0 {
			poll := ctx.Poll(frame.Question).AddOptions(frame.Options...)
			if frame.Selectable > 1 {
				poll = poll.MultiChoice()
			}
			msgID, err := poll.ReplyWithID()
			d.sendAck(stdinPipe, err == nil, string(msgID), err)
		}

	case "loader":
		if frame.Text != "" {
			ctx.StartAutoLoader()
		} else {
			ctx.StopAutoLoader()
		}

	case "done":
		return io.EOF

	default:
		logger.Debug("external plugin: unknown action frame", "action", frame.Action)
	}

	return nil
}

func (d *Dispatcher) sendAck(stdinPipe io.WriteCloser, ok bool, msgID string, err error) {
	if stdinPipe == nil {
		return
	}
	ack := Ack{OK: ok, MsgID: msgID}
	if err != nil {
		ack.Error = err.Error()
	}
	data, _ := json.Marshal(ack)
	_, _ = fmt.Fprintf(stdinPipe, "%s\n", data)
}

// resolveMediaData parses base64 media payload or downloads from HTTP/HTTPS URL.
func resolveMediaData(raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("media data is empty")
	}

	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		client := &http.Client{Timeout: 15 * time.Second}
		resp, err := client.Get(raw)
		if err != nil {
			return nil, fmt.Errorf("download media failed: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("download media returned status %d", resp.StatusCode)
		}
		return io.ReadAll(io.LimitReader(resp.Body, MaxPluginBinarySize))
	}

	// Remove data URI prefix if present (e.g. "data:image/png;base64,...")
	if idx := strings.Index(raw, ";base64,"); idx != -1 {
		raw = raw[idx+8:]
	}

	return base64.StdEncoding.DecodeString(raw)
}
