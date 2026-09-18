package owner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"

	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"whatsrook"
	"whatsrook/cmd/dispatch"
	"whatsrook/util/logger"
	"whatsrook/util/media"
)

func init() {
	dispatch.RegisterPreInterceptor("owner_shell", func(c *dispatch.Context, text string) bool {
		return HandleShellInput(c, text)
	})
	dispatch.Register(&dispatch.Command{
		Name:        "logout",
		Alias:       "unpair",
		Description: "Log out WhatsApp session, revoke pairing, and purge credentials (sudoers only)",
		Category:    "owner",
		IsPublic:    false,
		Handler:     handleLogoutCommand,
	})
	dispatch.Register(&dispatch.Command{
		Name:        "presence",
		Alias:       "online,setonline,active",
		Description: "Set or refresh client presence to online / browser active (sudoers only)",
		Category:    "owner",
		IsPublic:    false,
		Handler:     handlePresence,
	})
	dispatch.Register(&dispatch.Command{
		Name:        "bio",
		Alias:       "setbio",
		Description: "Update the bot's WhatsApp status bio message",
		Category:    "owner",
		IsPublic:    false,
		Handler:     handleBio,
	})
	dispatch.Register(&dispatch.Command{
		Name:        "blocklist",
		Alias:       "blocks",
		Description: "Display list of all currently blocked contacts",
		Category:    "chats",
		IsPublic:    false,
		Handler:     handleBlocklist,
	})
	dispatch.Register(&dispatch.Command{
		Name:        "pp",
		Alias:       "setpp",
		Description: "Update the bot's WhatsApp profile picture (replying to an image or image upload)",
		Category:    "owner",
		IsPublic:    false,
		Handler:     handleSetBotPP,
	})
	dispatch.Register(&dispatch.Command{
		Name:        "sh",
		Alias:       "exec,ps,cmd,shell,bash",
		Description: "Execute a shell command with real-time log streaming and stdin input (sudoers only).",
		Category:    "owner",
		IsPublic:    false,
		Handler:     handleSh,
	})
	dispatch.Register(&dispatch.Command{
		Name:        "stop",
		Alias:       "kill",
		Description: "Stop/terminate any running interactive shell session in this chat",
		Category:    "owner",
		IsPublic:    false,
		Handler:     handleStopShell,
	})
	dispatch.Register(&dispatch.Command{
		Name:        "status",
		Alias:       "poststatus",
		Description: "Post a status update (text or media) to WhatsApp status broadcast",
		Category:    "owner",
		IsPublic:    false,
		Handler:     handleStatus,
	})
	dispatch.Register(&dispatch.Command{
		Name:        "setsudo",
		Alias:       "sudo,addsudo",
		Description: "Add a user to the sudo list (replied user or numbers)",
		Category:    "owner",
		IsPublic:    false,
		Handler:     handleSetSudo,
	})
	dispatch.Register(&dispatch.Command{
		Name:        "delsudo",
		Description: "Remove a user from the sudo list (replied user or numbers)",
		Category:    "owner",
		IsPublic:    false,
		Handler:     handleDelSudo,
	})
	dispatch.Register(&dispatch.Command{
		Name:        "listsudo",
		Description: "List all sudo users",
		Category:    "owner",
		IsPublic:    false,
		Handler:     handleListSudo,
	})
	dispatch.Register(&dispatch.Command{
		Name:        "ban",
		Description: "Block a user from using the bot commands (replied user or numbers)",
		Category:    "owner",
		IsPublic:    false,
		Handler:     handleBan,
	})
	dispatch.Register(&dispatch.Command{
		Name:        "unban",
		Description: "Unblock a user (replied user or numbers)",
		Category:    "owner",
		IsPublic:    false,
		Handler:     handleUnban,
	})
	dispatch.Register(&dispatch.Command{
		Name:        "mode",
		Description: "Toggle bot mode (public/private)",
		Category:    "owner",
		IsPublic:    false,
		Handler:     handleMode,
	})
}

func handleBio(ctx *dispatch.Context) error {
	if len(ctx.Args) == 0 {
		p := ctx.GetPrefix()
		return ctx.Replyf("Usage: %sbio <new WhatsApp status bio text>\n\nExample: %sbio Available | WhatsRook AI Bot", p, p)
	}

	newBio := ctx.RawArgs
	err := ctx.Client.SetStatusMessage(ctx.Ctx, types.SetStatusInput{Text: &newBio})
	if err != nil {
		return ctx.Replyf("Failed to update status bio: %v", err)
	}

	return ctx.Replyf("Bot status bio successfully updated to:\n\"%s\"", newBio)
}

func handleBlocklist(ctx *dispatch.Context) error {
	bl, err := ctx.Client.GetBlocklist(ctx.Ctx)
	if err != nil || bl == nil {
		return ctx.Replyf("Failed to fetch blocklist: %v", err)
	}

	if len(bl.JIDs) == 0 {
		return ctx.Reply("Your blocklist is currently empty.")
	}

	tb := ctx.Text().Headerf("BLOCKED CONTACTS (%d total)", len(bl.JIDs))

	var mentions []types.JID
	for i, jid := range bl.JIDs {
		bare := jid.ToNonAD()
		mentions = append(mentions, bare)
		tb.Numberedf(i+1, "+%s (@%s)", bare.User, bare.User)
	}

	p := ctx.GetPrefix()
	tb.Blank().Linef("To unblock a contact: %sunblock @user", p)

	return ctx.ReplyWithMentions(tb.String(), mentions)
}

func handleSetBotPP(ctx *dispatch.Context) error {
	downloadable, _, _ := whatsrook.ExtractMediaFromEvent(ctx.Evt)
	if downloadable == nil {
		return ctx.Replyf("Please upload or reply to an image to set as profile picture. Usage: %spp", ctx.GetPrefix())
	}

	rawBytes, err := ctx.Client.Download(ctx.Ctx, downloadable)
	if err != nil || len(rawBytes) == 0 {
		return ctx.Replyf("Failed to download image: %v", err)
	}

	jpegData, errConv := media.EnsureJPEG(ctx.Ctx, rawBytes)
	if errConv != nil || len(jpegData) == 0 {
		return ctx.Replyf("Failed to process profile image format: %v", errConv)
	}

	ownJID := types.EmptyJID
	if ctx.Client != nil && ctx.Client.Store != nil && ctx.Client.Store.ID != nil {
		ownJID = ctx.Client.Store.ID.ToNonAD()
	}

	logger.Info("handleSetBotPP: Setting bot profile picture", "rawBytes", len(rawBytes), "jpegBytes", len(jpegData), "targetJID", ownJID.String())
	picID, errSet := ctx.Client.SetGroupPhoto(ctx.Ctx, ownJID, jpegData)
	if errSet != nil {
		logger.Error("handleSetBotPP failed", "err", errSet)
		return ctx.Replyf("Failed to update profile picture: %v", errSet)
	}

	return ctx.Replyf("Bot profile picture updated successfully! (Picture ID: %s)", picID)
}

func handleStopShell(ctx *dispatch.Context) error {
	ActiveShellSessionsMu.Lock()
	session, exists := ActiveShellSessions[ctx.Chat.String()]
	ActiveShellSessionsMu.Unlock()

	if !exists || session == nil {
		return ctx.Reply("No active shell session running in this chat.")
	}

	session.Mu.Lock()
	session.UserTerminated = true
	cancel := session.Cancel
	session.Mu.Unlock()

	if cancel != nil {
		cancel()
	}
	return ctx.Reply("Shell command terminated.")
}

// HandleShellInput checks if there is an active shell session in the chat and feeds stdin input to it.
func HandleShellInput(ctx *dispatch.Context, text string) bool {
	if text == "" {
		return false
	}

	ActiveShellSessionsMu.Lock()
	session, exists := ActiveShellSessions[ctx.Chat.String()]
	ActiveShellSessionsMu.Unlock()

	if !exists || session == nil {
		return false
	}

	if !ctx.IsSudo() {
		return false
	}

	trimmed := strings.TrimSpace(text)
	lower := strings.ToLower(trimmed)

	// Check if this is a command to stop the shell session
	if lower == ".stop" || lower == "!stop" || lower == "stop" || lower == "cancel" || lower == "exit" || lower == "^c" || lower == "ctrl+c" {
		session.Mu.Lock()
		session.UserTerminated = true
		cancel := session.Cancel
		session.Mu.Unlock()
		if cancel != nil {
			cancel()
			_ = ctx.Reply("Shell command terminated.")
			return true
		}
	}

	// If the text starts with a registered command prefix other than stop, don't capture as stdin
	var prefixes []string
	if s, ok := dispatch.GetSQLStore(ctx.Client); ok {
		if p, err := s.GetSetting(ctx.Ctx, "prefix"); err == nil && strings.TrimSpace(p) != "" {
			prefixes = strings.Fields(p)
		}
	}
	if len(prefixes) == 0 {
		prefixes = []string{"."}
	}
	for _, p := range prefixes {
		if p != "" && strings.HasPrefix(trimmed, p) {
			body := strings.TrimSpace(trimmed[len(p):])
			fields := strings.Fields(body)
			if len(fields) > 0 {
				cmdWord := strings.ToLower(fields[0])
				if _, isCmd := dispatch.Get(cmdWord); isCmd {
					return false
				}
			}
		}
	}

	// Feed input into stdin
	session.Mu.Lock()
	defer session.Mu.Unlock()

	if session.Stdin != nil {
		_, err := session.Stdin.Write([]byte(text + "\n"))
		if err != nil {
			_ = ctx.Replyf("Failed to write to stdin: %v", err)
			return true
		}
		session.Buf.WriteString(dispatch.Sprintf("\n[stdin] %s\n", text))
		select {
		case session.UpdateCh <- struct{}{}:
		default:
		}
		return true
	}

	return false
}

func handleSh(ctx *dispatch.Context) error {
	commandStr := strings.TrimSpace(ctx.RawArgs)
	if commandStr == "" {
		p := ctx.GetPrefix()
		if runtime.GOOS == "windows" {
			return ctx.Replyf("Usage: %ssh <command line>\n\nExample:\n%ssh dir\n%ssh git status\n%ssh yt-dlp \"https://...\" -t mp4", p, p, p, p)
		}
		return ctx.Replyf("Usage: %ssh <command line>\n\nExample:\n%ssh yt-dlp \"https://...\" -t mp4\n%ssh ls -la", p, p, p)
	}

	chatKey := ctx.Chat.String()

	// Cancel any active session in this chat
	ActiveShellSessionsMu.Lock()
	if old, exists := ActiveShellSessions[chatKey]; exists && old != nil {
		old.Mu.Lock()
		old.UserTerminated = true
		if old.Cancel != nil {
			old.Cancel()
		}
		old.Mu.Unlock()
	}
	ActiveShellSessionsMu.Unlock()

	execCtx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		psScript := "[Console]::OutputEncoding = [util.Text.Encoding]::UTF8; " + commandStr
		if pwshPath, err := exec.LookPath("pwsh"); err == nil {
			cmd = exec.CommandContext(execCtx, pwshPath, "-NoProfile", "-NonInteractive", "-Command", psScript)
		} else if psPath, err := exec.LookPath("powershell"); err == nil {
			cmd = exec.CommandContext(execCtx, psPath, "-NoProfile", "-NonInteractive", "-Command", psScript)
		} else {
			cmd = exec.CommandContext(execCtx, "cmd.exe", "/c", commandStr)
		}
		cmd.Env = append(os.Environ(),
			"PYTHONUNBUFFERED=1",
			"CI=1",
		)
	} else {
		shell := "bash"
		if _, err := exec.LookPath("bash"); err != nil {
			shell = "sh"
		}

		// If stdbuf exists, use it to force unbuffered / line-buffered stdout and stderr
		execCmdStr := commandStr
		if _, err := exec.LookPath("stdbuf"); err == nil {
			execCmdStr = "stdbuf -oL -eL " + commandStr
		}

		cmd = exec.CommandContext(execCtx, shell, "-c", execCmdStr)
		cmd.Env = append(os.Environ(),
			"TERM=xterm-256color",
			"PYTHONUNBUFFERED=1",
			"FORCE_COLOR=1",
			"CLICOLOR_FORCE=1",
			"CI=1",
			"HOMEBREW_NO_AUTO_UPDATE=1",
		)
	}

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return ctx.Replyf("Failed to open stdin pipe: %v", err)
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return ctx.Replyf("Failed to open stdout pipe: %v", err)
	}

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return ctx.Replyf("Failed to open stderr pipe: %v", err)
	}

	initialMsg := dispatch.Sprintf("*Executing Shell Command...*\n`%s`\n\n```\n(starting process...)\n```\n_Type in chat to send stdin input. Type `.stop` to kill._", commandStr)
	msgID, err := ctx.ReplyWithID(initialMsg)
	if err != nil {
		cancel()
		return errors.New("failed to send initial shell message: " + err.Error())
	}

	session := &ShellSession{
		Chat:       ctx.Chat,
		Sender:     ctx.Sender,
		MsgID:      msgID,
		Cmd:        cmd,
		Stdin:      stdinPipe,
		Cancel:     cancel,
		Buf:        new(bytes.Buffer),
		StartTime:  time.Now(),
		CommandStr: commandStr,
		UpdateCh:   make(chan struct{}, 20),
		Done:       make(chan struct{}),
	}

	ActiveShellSessionsMu.Lock()
	ActiveShellSessions[chatKey] = session
	ActiveShellSessionsMu.Unlock()

	if err := cmd.Start(); err != nil {
		ActiveShellSessionsMu.Lock()
		delete(ActiveShellSessions, chatKey)
		ActiveShellSessionsMu.Unlock()
		cancel()
		_, _ = ctx.Edit(msgID, dispatch.Sprintf("*Shell Error:*\n`%s`\n\n```\nFailed to start: %v\n```", commandStr, err))
		return nil
	}

	// Concurrent stdout and stderr readers
	var readWg sync.WaitGroup
	readStream := func(r io.Reader) {
		defer readWg.Done()
		buf := make([]byte, 1024)
		for {
			n, rErr := r.Read(buf)
			if n > 0 {
				session.Mu.Lock()
				session.Buf.Write(buf[:n])
				session.Mu.Unlock()

				select {
				case session.UpdateCh <- struct{}{}:
				default:
				}
			}
			if rErr != nil {
				return
			}
		}
	}

	readWg.Add(2)
	go readStream(stdoutPipe)
	go readStream(stderrPipe)

	// Debounced UI updater
	go func() {
		ticker := time.NewTicker(1200 * time.Millisecond)
		defer ticker.Stop()

		var lastEditedText string
		var lastEditTime time.Time

		doEdit := func() {
			session.Mu.Lock()
			rawOutput := session.Buf.String()
			session.Mu.Unlock()

			cleaned := CleanShellOutput(rawOutput)
			if cleaned == "" {
				cleaned = "(running...)"
			}

			if len(cleaned) > 3500 {
				cleaned = "... (truncated)\n" + cleaned[len(cleaned)-3400:]
			}

			if cleaned != lastEditedText && time.Since(lastEditTime) >= 800*time.Millisecond {
				updateText := dispatch.Sprintf("*Executing Shell Command...*\n`%s`\n\n```\n%s\n```\n_Type in chat to send stdin input. Type `.stop` to kill._", session.CommandStr, cleaned)
				_, _ = ctx.Edit(session.MsgID, updateText)
				lastEditedText = cleaned
				lastEditTime = time.Now()
			}
		}

		for {
			select {
			case <-session.Done:
				return
			case <-session.UpdateCh:
				doEdit()
			case <-ticker.C:
				doEdit()
			}
		}
	}()

	// Background process waiter
	go func() {
		waitErr := cmd.Wait()
		readWg.Wait()
		duration := time.Since(session.StartTime).Round(100 * time.Millisecond)
		close(session.Done)

		ActiveShellSessionsMu.Lock()
		delete(ActiveShellSessions, chatKey)
		ActiveShellSessionsMu.Unlock()

		session.Mu.Lock()
		rawOutput := session.Buf.String()
		userKilled := session.UserTerminated
		session.Mu.Unlock()

		cancel()

		cleaned := CleanShellOutput(rawOutput)
		if cleaned == "" {
			cleaned = "(no output)"
		}

		statusStr := dispatch.Sprintf("Success (exited in %s)", duration)
		if userKilled {
			statusStr = dispatch.Sprintf("Terminated by user (ran for %s)", duration)
		} else if execCtx.Err() == context.DeadlineExceeded {
			statusStr = dispatch.Sprintf("Timed out after %s", duration)
		} else if waitErr != nil {
			statusStr = dispatch.Sprintf("Failed: %v (ran for %s)", waitErr, duration)
		}

		if len(cleaned) > 3500 {
			cleaned = "... (truncated)\n" + cleaned[len(cleaned)-3400:]
		}

		finalMsg := dispatch.Sprintf("*Shell Output*\nCommand: `%s`\nStatus: *%s*\n\n```\n%s\n```", session.CommandStr, statusStr, cleaned)
		_, _ = ctx.Edit(session.MsgID, finalMsg)
	}()

	return nil
}

func handleStatus(ctx *dispatch.Context) error {
	text := strings.TrimSpace(ctx.RawArgs)

	mediaBytes, mimetype, err := ctx.GetMedia()
	if err == nil && len(mediaBytes) > 0 {
		isImage := strings.HasPrefix(mimetype, "image")
		isVideo := strings.HasPrefix(mimetype, "video") || strings.Contains(mimetype, "gif")

		if !isImage && !isVideo {
			if strings.HasPrefix(mimetype, "audio") {
				return ctx.Reply("Only image and video media can be posted to status broadcast.")
			}
			isImage = true
		}

		if isImage {
			if mimetype == "" {
				mimetype = "image/jpeg"
			}
			uploaded, uErr := ctx.Client.Upload(ctx.Ctx, mediaBytes, whatsmeow.MediaImage)
			if uErr != nil {
				logger.Error("handleStatus: image upload failed", "err", uErr)
				return ctx.Replyf("Failed to upload status image: %v", uErr)
			}
			msg := &waE2E.Message{
				ImageMessage: &waE2E.ImageMessage{
					URL:           &uploaded.URL,
					DirectPath:    &uploaded.DirectPath,
					MediaKey:      uploaded.MediaKey,
					Mimetype:      &mimetype,
					FileEncSHA256: uploaded.FileEncSHA256,
					FileSHA256:    uploaded.FileSHA256,
					FileLength:    new(uint64),
				},
			}
			*msg.ImageMessage.FileLength = uint64(len(mediaBytes))
			if text != "" {
				msg.ImageMessage.Caption = &text
			}

			_, sendErr := ctx.Client.SendMessage(ctx.Ctx, types.StatusBroadcastJID, msg)
			if sendErr != nil {
				logger.Error("handleStatus: send image status failed", "err", sendErr)
				return ctx.Replyf("Failed to post image status: %v", sendErr)
			}
			return ctx.Reply("Successfully posted image status update.")
		}

		if isVideo {
			if mimetype == "" {
				mimetype = "video/mp4"
			}
			uploaded, uErr := ctx.Client.Upload(ctx.Ctx, mediaBytes, whatsmeow.MediaVideo)
			if uErr != nil {
				logger.Error("handleStatus: video upload failed", "err", uErr)
				return ctx.Replyf("Failed to upload status video: %v", uErr)
			}
			msg := &waE2E.Message{
				VideoMessage: &waE2E.VideoMessage{
					URL:           &uploaded.URL,
					DirectPath:    &uploaded.DirectPath,
					MediaKey:      uploaded.MediaKey,
					Mimetype:      &mimetype,
					FileEncSHA256: uploaded.FileEncSHA256,
					FileSHA256:    uploaded.FileSHA256,
					FileLength:    new(uint64),
				},
			}
			*msg.VideoMessage.FileLength = uint64(len(mediaBytes))
			if text != "" {
				msg.VideoMessage.Caption = &text
			}

			_, sendErr := ctx.Client.SendMessage(ctx.Ctx, types.StatusBroadcastJID, msg)
			if sendErr != nil {
				logger.Error("handleStatus: send video status failed", "err", sendErr)
				return ctx.Replyf("Failed to post video status: %v", sendErr)
			}
			return ctx.Reply("Successfully posted video status update.")
		}
	}

	if text == "" {
		p := ctx.GetPrefix()
		return ctx.Replyf("Usage:\n- %sstatus <text>\n- Reply to image/video with %sstatus [optional caption]", p, p)
	}

	msg := &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: &text,
		},
	}

	_, sendErr := ctx.Client.SendMessage(ctx.Ctx, types.StatusBroadcastJID, msg)
	if sendErr != nil {
		logger.Error("handleStatus: send text status failed", "err", sendErr)
		return ctx.Replyf("Failed to post text status: %v", sendErr)
	}
	return ctx.Reply("Successfully posted text status update.")
}

// resolveUserTokens resolves a target JID into all known identity tokens:
// the phone-number JID string, LID JID string, and username (or push name if no username exists).
// It does not save the bare phone number. It also persists any newly discovered LID↔PN mapping
// into the LID store so IsSameUserRaw can resolve them in future comparisons.
func resolveUserTokens(ctx context.Context, client *whatsmeow.Client, chat, target types.JID) (tokens []string, mentionJID types.JID, displayUser string) {
	nonAD := target.ToNonAD()

	var pnJID, lidJID types.JID

	switch nonAD.Server {
	case types.HiddenUserServer:
		lidJID = nonAD
		// Resolve LID → PN from the LID store.
		if client != nil && client.Store != nil && client.Store.LIDs != nil {
			if pn, err := client.Store.LIDs.GetPNForLID(ctx, nonAD); err == nil && !pn.IsEmpty() {
				pnJID = pn.ToNonAD()
			}
		}
		// Fallback: check recent message store for SenderAlt
		if pnJID.IsEmpty() {
			if recent := whatsrook.GetRecentMessageForJID(nonAD); recent != nil && !recent.Info.SenderAlt.IsEmpty() && recent.Info.SenderAlt.Server == types.DefaultUserServer {
				pnJID = recent.Info.SenderAlt.ToNonAD()
			} else if recent := whatsrook.GetRecentMessageForJID(chat); recent != nil && !recent.Info.SenderAlt.IsEmpty() && recent.Info.SenderAlt.Server == types.DefaultUserServer {
				pnJID = recent.Info.SenderAlt.ToNonAD()
			}
		}
		// Fallback: query group participants to find the PN for this LID.
		if pnJID.IsEmpty() && chat.Server == "g.us" && client != nil {
			if info, err := client.GetGroupInfo(ctx, chat); err == nil {
				for _, p := range info.Participants {
					pLID := p.LID.ToNonAD()
					if !pLID.IsEmpty() && pLID.User == nonAD.User {
						pnJID = p.JID.ToNonAD()
						break
					}
				}
			}
		}
		// Fallback: query GetUserInfo
		if pnJID.IsEmpty() && client != nil {
			if uMap, err := client.GetUserInfo(ctx, []types.JID{nonAD}); err == nil && uMap != nil {
				if uInfo, ok := uMap[nonAD]; ok && !uInfo.LID.IsEmpty() && uInfo.LID != nonAD {
					pnJID = uInfo.LID.ToNonAD()
				}
			}
		}
		// Persist the discovered LID↔PN mapping.
		if !pnJID.IsEmpty() && client != nil && client.Store != nil && client.Store.LIDs != nil {
			_ = client.Store.LIDs.PutLIDMapping(ctx, lidJID, pnJID)
		}
	default:
		pnJID = nonAD
		// Resolve PN → LID from the LID store.
		if client != nil && client.Store != nil && client.Store.LIDs != nil {
			if lid, err := client.Store.LIDs.GetLIDForPN(ctx, nonAD); err == nil && !lid.IsEmpty() {
				lidJID = lid.ToNonAD()
			}
		}
		// Fallback: check recent message store
		if lidJID.IsEmpty() {
			if recent := whatsrook.GetRecentMessageForJID(nonAD); recent != nil && recent.Info.Sender.Server == types.HiddenUserServer {
				lidJID = recent.Info.Sender.ToNonAD()
			} else if recent := whatsrook.GetRecentMessageForJID(chat); recent != nil && recent.Info.Sender.Server == types.HiddenUserServer {
				lidJID = recent.Info.Sender.ToNonAD()
			}
		}
		// Fallback: query group participants.
		if lidJID.IsEmpty() && chat.Server == "g.us" && client != nil {
			if info, err := client.GetGroupInfo(ctx, chat); err == nil {
				for _, p := range info.Participants {
					pPN := p.JID.ToNonAD()
					if !pPN.IsEmpty() && pPN.User == nonAD.User {
						lidJID = p.LID.ToNonAD()
						break
					}
				}
			}
		}
		// Fallback: query GetUserInfo
		if lidJID.IsEmpty() && client != nil {
			if uMap, err := client.GetUserInfo(ctx, []types.JID{nonAD}); err == nil && uMap != nil {
				if uInfo, ok := uMap[nonAD]; ok && !uInfo.LID.IsEmpty() {
					lidJID = uInfo.LID.ToNonAD()
				}
			}
		}
		// Persist the discovered LID↔PN mapping.
		if !lidJID.IsEmpty() && client != nil && client.Store != nil && client.Store.LIDs != nil {
			_ = client.Store.LIDs.PutLIDMapping(ctx, lidJID, pnJID)
		}
	}

	seen := make(map[string]bool)
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			tokens = append(tokens, s)
		}
	}

	if !pnJID.IsEmpty() {
		add(pnJID.String()) // e.g. "2348000000000@s.whatsapp.net"
	}
	if !lidJID.IsEmpty() {
		add(lidJID.String()) // e.g. "123456789012345@lid"
	}

	// Resolve username (only if it's a valid username token without spaces).
	if client != nil && client.Store != nil && client.Store.Contacts != nil {
		lookupJID := pnJID
		if lookupJID.IsEmpty() {
			lookupJID = lidJID
		}
		if !lookupJID.IsEmpty() {
			if contact, err := client.Store.Contacts.GetContact(ctx, lookupJID); err == nil && contact.Found {
				if contact.Username != "" {
					u := strings.TrimSpace(strings.ToLower(contact.Username))
					if isValidUsername(u) {
						add(u)
					}
				}
			}
		}
	}

	// Fallback: check recent message push name ONLY if it's a valid single-word username
	if recent := whatsrook.GetRecentMessageForJID(nonAD); recent != nil && recent.Info.PushName != "" {
		p := strings.TrimSpace(strings.ToLower(recent.Info.PushName))
		if isValidUsername(p) {
			add(p)
		}
	} else if recent := whatsrook.GetRecentMessageForJID(chat); recent != nil && recent.Info.PushName != "" {
		p := strings.TrimSpace(strings.ToLower(recent.Info.PushName))
		if isValidUsername(p) {
			add(p)
		}
	}

	// If we have no tokens at all, fall back to the raw non-AD string.
	if len(tokens) == 0 {
		add(nonAD.String())
	}

	// Choose the best JID for mention display purposes.
	if !pnJID.IsEmpty() && pnJID.User != "" {
		mentionJID = pnJID
		displayUser = pnJID.User
	} else if nonAD.User != "" {
		mentionJID = nonAD
		displayUser = nonAD.User
	} else {
		mentionJID = nonAD
		displayUser = nonAD.String()
	}
	return tokens, mentionJID, displayUser
}

// removeUserTokens removes all tokens associated with target from the given token slice.
// It resolves all identity tokens for target and removes any matching entries.
func removeUserTokens(ctx context.Context, client *whatsmeow.Client, chat types.JID, existing []string, targets []types.JID) (newList []string, removedJIDs []types.JID, displayNames []string) {
	// Build a set of all tokens for all targets.
	type targetMeta struct {
		tokens      map[string]bool
		mentionJID  types.JID
		displayUser string
	}
	targetMetas := make([]targetMeta, 0, len(targets))
	for _, t := range targets {
		toks, mentionJID, displayUser := resolveUserTokens(ctx, client, chat, t)
		tokSet := make(map[string]bool, len(toks))
		for _, tok := range toks {
			tokSet[tok] = true
		}
		targetMetas = append(targetMetas, targetMeta{tokSet, mentionJID, displayUser})
	}

	// Track which targets were actually matched.
	matched := make([]bool, len(targets))

	for _, entry := range existing {
		entryMatched := false
		for i, tm := range targetMetas {
			if tm.tokens[entry] {
				entryMatched = true
				if !matched[i] {
					matched[i] = true
					removedJIDs = append(removedJIDs, tm.mentionJID)
					displayNames = append(displayNames, "@"+tm.displayUser)
				}
				break
			}
			// Also try parsing the stored token as a JID and using IsSameUserRaw.
			if strings.Contains(entry, "@") {
				if stored, err := types.ParseJID(entry); err == nil && stored.User != "" {
					if whatsrook.IsSameUserRaw(ctx, client, targets[i], stored) {
						entryMatched = true
						if !matched[i] {
							matched[i] = true
							removedJIDs = append(removedJIDs, tm.mentionJID)
							displayNames = append(displayNames, "@"+tm.displayUser)
						}
						break
					}
				}
			}
		}
		if !entryMatched {
			newList = append(newList, entry)
		}
	}
	return newList, removedJIDs, displayNames
}

func handleSetSudo(ctx *dispatch.Context) error {
	targets := ctx.GetTargets()
	if len(targets) == 0 {
		p := ctx.GetPrefix()
		return ctx.Replyf("Usage:\n- %ssetsudo @user\n- %ssetsudo 1234567890\n- Reply to a user's message with %ssetsudo", p, p, p)
	}

	s, ok := dispatch.GetStore(ctx)
	if !ok {
		return ctx.Reply("Settings store unavailable.")
	}

	raw, err := s.GetSetting(ctx.Ctx, "sudoers")
	if err != nil {
		return err
	}

	sudoers := sanitizeSudoers(strings.Fields(raw))
	var addedJIDs []types.JID
	var displayNames []string

	for _, target := range targets {
		newTokens, mentionJID, displayUser := resolveUserTokens(ctx.Ctx, ctx.Client, ctx.Chat, target)
		var addedThisTarget bool
		for _, tok := range newTokens {
			if !slices.Contains(sudoers, tok) {
				sudoers = append(sudoers, tok)
				addedThisTarget = true
			}
			_ = s.PutSetting(ctx.Ctx, "sudo:"+tok, "true")
		}
		if !target.IsEmpty() {
			_ = s.PutSetting(ctx.Ctx, "sudo:"+target.ToNonAD().String(), "true")
			_ = s.PutSetting(ctx.Ctx, "sudo:"+target.ToNonAD().User, "true")
		}
		if addedThisTarget {
			addedJIDs = append(addedJIDs, mentionJID)
			displayNames = append(displayNames, "@"+displayUser)
		}
	}

	if len(addedJIDs) == 0 {
		return ctx.Reply("Target(s) already in the sudo list.")
	}

	if err := s.PutSetting(ctx.Ctx, "sudoers", strings.Join(sudoers, " ")); err != nil {
		logger.Error("handleSetSudo: PutSetting failed", "err", err, "sudoers", sudoers)
		return ctx.Replyf("Failed to update sudoers list: %v", err)
	}

	return ctx.ReplyWithMentions(dispatch.Sprintf("Added to sudo: %s", strings.Join(displayNames, ", ")), addedJIDs)
}

func handleDelSudo(ctx *dispatch.Context) error {
	if !ctx.IsOwner() {
		return ctx.Reply("Only the bot owner can remove users from the sudo list.")
	}

	targets := ctx.GetTargets()
	if len(targets) == 0 {
		p := ctx.GetPrefix()
		return ctx.Replyf("Usage:\n- %sdelsudo @user\n- %sdelsudo 1234567890\n- Reply to a user's message with %sdelsudo", p, p, p)
	}
	if slices.ContainsFunc(targets, ctx.IsTargetOwner) {
		return ctx.Reply("Cannot remove the bot owner from sudoers.")
	}

	s, ok := dispatch.GetStore(ctx)
	if !ok {
		return ctx.Reply("Settings store unavailable.")
	}

	raw, err := s.GetSetting(ctx.Ctx, "sudoers")
	if err != nil {
		return err
	}

	sudoers := sanitizeSudoers(strings.Fields(raw))
	newSudoers, removedJIDs, displayNames := removeUserTokens(ctx.Ctx, ctx.Client, ctx.Chat, sudoers, targets)

	if len(removedJIDs) == 0 {
		return ctx.Reply("Target(s) not found in the sudo list.")
	}

	for _, target := range targets {
		toks, _, _ := resolveUserTokens(ctx.Ctx, ctx.Client, ctx.Chat, target)
		for _, tok := range toks {
			_ = s.DeleteSetting(ctx.Ctx, "sudo:"+tok)
		}
		_ = s.DeleteSetting(ctx.Ctx, "sudo:"+target.ToNonAD().String())
		_ = s.DeleteSetting(ctx.Ctx, "sudo:"+target.ToNonAD().User)
	}

	if err := s.PutSetting(ctx.Ctx, "sudoers", strings.Join(newSudoers, " ")); err != nil {
		logger.Error("handleDelSudo: PutSetting failed", "err", err, "newSudoers", newSudoers)
		return ctx.Replyf("Failed to update sudoers list: %v", err)
	}

	return ctx.ReplyWithMentions(dispatch.Sprintf("Removed from sudo: %s", strings.Join(displayNames, ", ")), removedJIDs)
}

func handleListSudo(ctx *dispatch.Context) error {

	s, ok := dispatch.GetStore(ctx)
	if !ok {
		return ctx.Reply("Settings store unavailable.")
	}

	raw, err := s.GetSetting(ctx.Ctx, "sudoers")
	if err != nil {
		return err
	}

	rawTokens := strings.Fields(raw)
	sudoers := sanitizeSudoers(rawTokens)
	if len(sudoers) != len(rawTokens) {
		_ = s.PutSetting(ctx.Ctx, "sudoers", strings.Join(sudoers, " "))
	}

	var mentions []types.JID
	tb := ctx.Text().Header("Sudo List")

	if ctx.Client.Store.ID != nil {
		ownerJID := ctx.Client.Store.ID.ToNonAD()
		resolvedJID, username := ctx.ResolveMention(ownerJID)
		if username == "" {
			username = ownerJID.User
		}
		tb.Bulletf("@%s (Owner)", username)
		mentions = append(mentions, resolvedJID)
	}

	// Deduplicate: track which tokens we've already displayed so that a user
	// stored as multiple tokens (PN JID + LID + bare number + username) appears only once.
	displayedTokens := make(map[string]bool)

	for _, sdr := range sudoers {
		if displayedTokens[sdr] {
			continue
		}

		var sudoerJID types.JID
		var isJID bool

		if strings.Contains(sdr, "@") {
			if parsed, err := types.ParseJID(sdr); err == nil && parsed.User != "" {
				sudoerJID = parsed.ToNonAD()
				isJID = true
			}
		} else if clean := strings.TrimPrefix(sdr, "+"); len(clean) >= 5 && isDigits(clean) {
			sudoerJID = types.NewJID(clean, types.DefaultUserServer)
			isJID = true
		}

		if !isJID {
			displayedTokens[sdr] = true
			continue
		}

		if ctx.IsTargetOwner(sudoerJID) {
			displayedTokens[sdr] = true
			continue
		}

		resolvedJID, username := ctx.ResolveMention(sudoerJID)
		if username == "" {
			username = sudoerJID.User
		}
		tb.Bulletf("@%s", username)
		mentions = append(mentions, resolvedJID)

		// Mark all tokens that resolve to the same identity as displayed.
		allToks, _, _ := resolveUserTokens(ctx.Ctx, ctx.Client, ctx.Chat, sudoerJID)
		for _, tok := range allToks {
			displayedTokens[tok] = true
		}
		displayedTokens[sdr] = true
	}

	return ctx.ReplyWithMentions(tb.String(), mentions)
}

func isValidUsername(s string) bool {
	if len(s) == 0 || len(s) > 30 {
		return false
	}
	if strings.ContainsAny(s, " \t\r\n@:+{}[]()|/\\") {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '.' && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

func isDigits(s string) bool {
	if len(s) == 0 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func sanitizeSudoers(tokens []string) []string {
	var sanitized []string
	seen := make(map[string]bool)
	for _, tok := range tokens {
		tok = strings.TrimSpace(tok)
		if tok == "" || seen[tok] {
			continue
		}
		if strings.Contains(tok, "@") {
			if parsed, err := types.ParseJID(tok); err == nil && parsed.User != "" {
				seen[tok] = true
				sanitized = append(sanitized, tok)
				continue
			}
		} else if clean := strings.TrimPrefix(tok, "+"); len(clean) >= 5 && isDigits(clean) {
			seen[tok] = true
			sanitized = append(sanitized, tok)
			continue
		} else if isValidUsername(tok) {
			seen[tok] = true
			sanitized = append(sanitized, tok)
			continue
		}
	}
	return sanitized
}

func handleBan(ctx *dispatch.Context) error {
	targets := ctx.GetTargets()
	if len(targets) == 0 {
		p := ctx.GetPrefix()
		return ctx.Replyf("Usage:\n- %sban @user\n- %sban 1234567890\n- Reply to a user's message with %sban", p, p, p)
	}

	s, ok := dispatch.GetStore(ctx)
	if !ok {
		return ctx.Reply("Settings store unavailable.")
	}

	rawBanned, err := s.GetSetting(ctx.Ctx, "banned_users")
	if err != nil {
		return err
	}
	bannedUsers := strings.Fields(rawBanned)

	var bannedJIDs []types.JID
	var displayNames []string

	for _, target := range targets {
		// Disallow banning the bot owner or sudoers.
		if ctx.IsTargetOwner(target) {
			continue
		}
		if whatsrook.IsSudoRaw(ctx.Ctx, ctx.Client, target) {
			continue
		}

		newTokens, mentionJID, displayUser := resolveUserTokens(ctx.Ctx, ctx.Client, ctx.Chat, target)
		var addedThisTarget bool
		for _, tok := range newTokens {
			if !slices.Contains(bannedUsers, tok) {
				bannedUsers = append(bannedUsers, tok)
				addedThisTarget = true
			}
			_ = s.PutSetting(ctx.Ctx, "ban:"+tok, "true")
		}
		if !target.IsEmpty() {
			_ = s.PutSetting(ctx.Ctx, "ban:"+target.ToNonAD().String(), "true")
			_ = s.PutSetting(ctx.Ctx, "ban:"+target.ToNonAD().User, "true")
		}
		if addedThisTarget {
			bannedJIDs = append(bannedJIDs, mentionJID)
			displayNames = append(displayNames, "@"+displayUser)
		}
	}

	if len(bannedJIDs) == 0 {
		return ctx.Reply("Target(s) could not be banned (already banned, owner, or sudo).")
	}

	if err := s.PutSetting(ctx.Ctx, "banned_users", strings.Join(bannedUsers, " ")); err != nil {
		return ctx.Reply("Failed to update banned users list.")
	}

	return ctx.ReplyWithMentions(dispatch.Sprintf("Banned from commands: %s", strings.Join(displayNames, ", ")), bannedJIDs)
}

func handleUnban(ctx *dispatch.Context) error {
	targets := ctx.GetTargets()
	if len(targets) == 0 {
		p := ctx.GetPrefix()
		return ctx.Replyf("Usage:\n- %sunban @user\n- %sunban 1234567890\n- Reply to a user's message with %sunban", p, p, p)
	}

	s, ok := dispatch.GetStore(ctx)
	if !ok {
		return ctx.Reply("Settings store unavailable.")
	}

	rawBanned, err := s.GetSetting(ctx.Ctx, "banned_users")
	if err != nil {
		return err
	}
	bannedUsers := strings.Fields(rawBanned)

	newBanned, unbannedJIDs, displayNames := removeUserTokens(ctx.Ctx, ctx.Client, ctx.Chat, bannedUsers, targets)

	if len(unbannedJIDs) == 0 {
		return ctx.Reply("Target(s) not found in the banned list.")
	}

	for _, target := range targets {
		toks, _, _ := resolveUserTokens(ctx.Ctx, ctx.Client, ctx.Chat, target)
		for _, tok := range toks {
			_ = s.DeleteSetting(ctx.Ctx, "ban:"+tok)
		}
		_ = s.DeleteSetting(ctx.Ctx, "ban:"+target.ToNonAD().String())
		_ = s.DeleteSetting(ctx.Ctx, "ban:"+target.ToNonAD().User)
	}

	if err := s.PutSetting(ctx.Ctx, "banned_users", strings.Join(newBanned, " ")); err != nil {
		return ctx.Reply("Failed to update banned users list.")
	}

	return ctx.ReplyWithMentions(dispatch.Sprintf("Unbanned from commands: %s", strings.Join(displayNames, ", ")), unbannedJIDs)
}

func handleMode(ctx *dispatch.Context) error {
	s, ok := dispatch.GetStore(ctx)
	if !ok {
		return ctx.Reply("Settings store unavailable.")
	}

	p := ctx.GetPrefix()
	if len(ctx.Args) == 0 {
		current, err := s.GetSetting(ctx.Ctx, "mode")
		if err != nil {
			return ctx.Reply("Failed to retrieve bot mode.")
		}
		if current == "" {
			current = "public"
		}
		return ctx.Replyf("Current bot mode: %s\n\nUsage:\n- %smode public\n- %smode private", current, p, p)
	}

	mode := strings.ToLower(ctx.Args[0])
	if mode != "public" && mode != "private" {
		return ctx.Reply("Invalid mode. Usage: mode [public/private]")
	}

	err := s.PutSetting(ctx.Ctx, "mode", mode)
	if err != nil {
		return ctx.Reply("Failed to update bot mode.")
	}

	return ctx.Replyf("Bot mode set to %s.", mode)
}

func handlePresence(ctx *dispatch.Context) error {
	p := ctx.GetPrefix()
	arg := ""
	if len(ctx.Args) > 0 {
		arg = strings.ToLower(strings.TrimSpace(ctx.Args[0]))
	}

	switch arg {
	case "off", "offline", "unavailable", "passive":
		if err := ctx.Client.SendPresence(ctx.Ctx, types.PresenceUnavailable); err != nil {
			return ctx.Replyf("Failed to set presence unavailable: %v", err)
		}
		_ = ctx.Client.SetPassive(ctx.Ctx, true)
		ctx.Client.SetForceActiveDeliveryReceipts(false)
		return ctx.Reply("Client presence set to offline / passive.")
	case "on", "online", "available", "active", "":
		if ctx.Client.Store != nil && len(ctx.Client.Store.PushName) == 0 {
			ctx.Client.Store.PushName = "WhatsRook"
		}
		ctx.Client.SetForceActiveDeliveryReceipts(true)
		if err := ctx.Client.SetPassive(ctx.Ctx, false); err != nil {
			logger.Warn("SetPassive failed", "err", err)
		}
		if err := ctx.Client.SendPresence(ctx.Ctx, types.PresenceAvailable); err != nil {
			return ctx.Replyf("Failed to set presence online: %v", err)
		}
		return ctx.Reply("Client presence set to online (browser active).")
	default:
		return dispatch.ErrUsage(p + "presence [online|offline]")
	}
}

func handleLogoutCommand(ctx *dispatch.Context) error {
	_ = ctx.Reply("Logging out and unpairing companion device...")

	go func() {
		time.Sleep(1 * time.Second)
		if ctx.Client != nil {
			logoutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := ctx.Client.Logout(logoutCtx); err != nil {
				logger.Warn("Remote logout command returned error", "err", err)
			}
			ctx.Client.Disconnect()
			if ctx.Client.Store != nil {
				_ = ctx.Client.Store.Delete(logoutCtx)
			}
			ctx.Client.DispatchEvent(&events.LoggedOut{
				OnConnect: false,
				Reason:    events.ConnectFailureLoggedOut,
			})
		}
	}()

	return nil
}
