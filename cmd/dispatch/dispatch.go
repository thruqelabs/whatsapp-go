// dispatch package provides the primary runtime message router and command execution pipeline.
//
// it intercepts incoming WhatsApp message events, parses message bodies against active command prefixes,
// checks chat permissions (public mode, group restrictions, admin requirements, sudo/owner authorization),
// and dispatches to registered native handlers or external plugins.
package dispatch

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"

	utils "whatsrook"
	"whatsrook/cmd/store"
	"whatsrook/util"
	"whatsrook/util/external"
	"whatsrook/util/logger"

	"go.mau.fi/whatsmeow"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

var (
	botStartTime   = time.Now()
	botStartTimeMu sync.RWMutex
)

// SetStartupTime sets the bot's startup time for stale command detection.
func SetStartupTime(t time.Time) {
	botStartTimeMu.Lock()
	defer botStartTimeMu.Unlock()
	botStartTime = t
}

// GetStartupTime returns the recorded bot startup time.
func GetStartupTime() time.Time {
	botStartTimeMu.RLock()
	defer botStartTimeMu.RUnlock()
	return botStartTime
}

// IsMessageBeforeStartup reports whether evt has a non-zero timestamp that occurred before the bot started running.
func IsMessageBeforeStartup(evt *events.Message) bool {
	if evt == nil || evt.Info.Timestamp.IsZero() {
		return false
	}
	return evt.Info.Timestamp.Before(GetStartupTime())
}

func isStale(evt *events.Message) bool {
	return IsMessageBeforeStartup(evt)
}

// Dispatch evaluates an incoming message event against all registered commands and routing middleware.
// returns true if the message was handled by a command or reactive route.
func Dispatch(ctx context.Context, client *whatsmeow.Client, evt *events.Message) bool {
	dispatchStart := time.Now()
	msgID := ""
	if evt != nil {
		msgID = evt.Info.ID
	}
	defer func() {
		dur := time.Since(dispatchStart)
		if dur > 1*time.Millisecond {
			logger.Debug("[PERF] Dispatch total execution", "msgID", msgID, "elapsed", dur)
		}
	}()
	RecordRecentMessage(evt)

	if evt == nil || evt.Message == nil || client == nil || client.Store == nil || !client.IsConnected() || !client.IsLoggedIn() {
		return false
	}

	if evt.Info.Category == "peer" {
		return false
	}

	chatStr := evt.Info.Chat.String()
	senderStr := evt.Info.Sender.String()
	text := utils.ExtractMessageText(evt)

	if strings.HasPrefix(strings.TrimSpace(text), "{") {
		var respJSON struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(text), &respJSON); err == nil && respJSON.ID != "" {
			text = respJSON.ID
		}
	}

	logger.Debug("Incoming message received", "chat", chatStr, "sender", senderStr, "is_from_me", evt.Info.IsFromMe, "text", text)

	// Reactive dispatch: list button responses and poll votes
	cctx := &Context{
		Ctx:    ctx,
		Client: client,
		Evt:    evt,
		Chat:   evt.Info.Chat,
		Sender: evt.Info.Sender,
	}

	displayText := extractInteractionDisplayText(evt)
	if text != "" {
		if utils.DispatchListSelection(cctx, text, displayText) {
			return true
		}
	}
	msgProto := utils.UnwrapMessageProto(evt.Message)
	if msgProto == nil {
		msgProto = evt.Message
	}
	if pollUpdate := msgProto.GetPollUpdateMessage(); pollUpdate != nil {
		targetID := ""
		if key := pollUpdate.GetPollCreationMessageKey(); key != nil {
			targetID = key.GetID()
		}
		logger.Debug("Dispatcher: incoming poll vote message detected",
			"targetPollMsgID", targetID,
			"chat", evt.Info.Chat.String(),
			"sender", evt.Info.Sender.String(),
		)
		if utils.DispatchPollVoteEvent(cctx, evt) {
			logger.Debug("Dispatcher: poll vote event successfully dispatched to reactive route",
				"targetPollMsgID", targetID,
				"chat", evt.Info.Chat.String(),
				"sender", evt.Info.Sender.String(),
			)
			return true
		}
		logger.Debug("Dispatcher: poll vote event has no matching reactive route",
			"targetPollMsgID", targetID,
			"chat", evt.Info.Chat.String(),
			"sender", evt.Info.Sender.String(),
		)
	}

	s, okStore := GetSQLStore(client)
	if okStore {
		store.InitTables(ctx, s.SQLStore)
	}

	if evt.Info.Chat.Server == "g.us" && okStore {
		go store.StoreGroupMessage(context.WithoutCancel(ctx), s.SQLStore, evt.Info.Chat, evt.Info.Sender)
	}

	// 1. Status Broadcast Auto-Save
	if (evt.Info.Chat.String() == "status@broadcast" || evt.Info.Chat.Server == "broadcast") && okStore {
		raw, _ := s.GetSetting(ctx, "autostatussave")
		if raw == "on" && client.Store.ID != nil {
			ownerJID := client.Store.ID.ToNonAD()
			_, _ = client.SendMessage(ctx, ownerJID, evt.Message)
		}
	}

	// 2. ViewOnce Auto-Save / Forwarding (autovv)
	if (evt.IsViewOnce || evt.IsViewOnceV2 || utils.IsViewOnceMessage(evt.Message)) && okStore {
		handleAutoViewOnce(ctx, client, s.SQLStore, evt)
	}

	// 3. Asynchronous Post-Processors (AutoRead, AutoReact)
	if okStore && !evt.Info.IsFromMe {
		postProcessorsMu.RLock()
		for _, pp := range postProcessors {
			go pp.fn(ctx, client, s, evt)
		}
		postProcessorsMu.RUnlock()
	}

	// 4. Sticker Commands
	if okStore {
		stk := msgProto.GetStickerMessage()
		if stk != nil {
			if isStale(evt) {
				logger.Debug("Skipping sticker command from message sent before bot startup",
					"timestamp", evt.Info.Timestamp,
					"startupTime", GetStartupTime(),
				)
				return true
			}
			if handleStickerCommand(ctx, client, s.SQLStore, evt, stk) {
				return true
			}
		}
	}

	// 5. Bot Tagged / Mention Proto
	if isBotMentioned(client, evt) && okStore {
		if isStale(evt) {
			logger.Debug("Skipping bot mention response from message sent before bot startup",
				"timestamp", evt.Info.Timestamp,
				"startupTime", GetStartupTime(),
			)
			return true
		}
		if mentionProto, err := s.GetSetting(ctx, "mention_proto"); err == nil && mentionProto != "" {
			if msg, err := utils.DecodeProtoMessage(mentionProto); err == nil {
				setReplyContextInfo(msg, evt)
				_, _ = client.SendMessage(ctx, evt.Info.Chat, msg)
				return true
			}
		}
	}

	if text == "" {
		return false
	}

	tPre := time.Now()
	prefixes := activePrefixes(ctx, client)
	logger.Debug("[PERF] Dispatch: activePrefixes", "msgID", msgID, "elapsed", time.Since(tPre), "prefixes", prefixes)

	isCommand := false
	matchedBody := ""
	matchedPrefix := ""
	hasEmpty := false

	for _, p := range prefixes {
		if p == "" {
			hasEmpty = true
			continue
		}
		if matchesPrefix(text, p) {
			body := strings.TrimLeft(strings.TrimSpace(text[len(p):]), ",:;! \t")
			fields := strings.Fields(body)
			if len(fields) > 0 {
				cmdName := strings.ToLower(fields[0])
				if clean := strings.TrimRight(cmdName, ",:;!? \t"); clean != "" {
					cmdName = clean
				}
				if _, exists := Get(cmdName); exists || external.DefaultDispatcher.IsInstalled(cmdName) || isLikelyCommandName(cmdName) {
					isCommand = true
					matchedBody = body
					matchedPrefix = p
					break
				}
			}
		}
	}

	if !isCommand && hasEmpty {
		body := strings.TrimSpace(text)
		fields := strings.Fields(body)
		if len(fields) > 0 {
			first := strings.ToLower(fields[0])
			if _, exists := Get(first); exists || external.DefaultDispatcher.IsInstalled(first) {
				isCommand = true
				matchedBody = body
				matchedPrefix = ""
			}
		}
	}

	// 6. Filters & BGM Trigger Words (Only evaluate for non-commands)
	if !isCommand && okStore {
		if isStale(evt) {
			logger.Debug("Skipping filters and BGM from message sent before bot startup",
				"timestamp", evt.Info.Timestamp,
				"startupTime", GetStartupTime(),
			)
			return true
		}
		tFilt := time.Now()
		handled := handleFiltersAndBGM(ctx, client, s.SQLStore, evt, text)
		durFilt := time.Since(tFilt)
		if durFilt > 1*time.Millisecond || handled {
			logger.Debug("[PERF] Dispatch: handleFiltersAndBGM", "msgID", msgID, "elapsed", durFilt, "handled", handled)
		}
		if handled {
			return true
		}
	}

	// 7. Run registered Pre-Interceptors (Group Moderation, AFK, Games, Shell)
	preInterceptorsMu.RLock()
	preList := make([]interceptorEntry, len(preInterceptors))
	copy(preList, preInterceptors)
	preInterceptorsMu.RUnlock()

	for _, it := range preList {
		tIt := time.Now()
		handled := it.fn(cctx, text)
		durIt := time.Since(tIt)
		if durIt > 1*time.Millisecond || handled {
			logger.Debug("[PERF] Dispatch: pre-interceptor", "name", it.name, "elapsed", durIt, "handled", handled)
		}
		if handled {
			return true
		}
	}

	// If message was matched as a command, execute it
	if isCommand {
		if isStale(evt) {
			logger.Debug("Skipping command from message sent before bot startup",
				"prefix", matchedPrefix,
				"body", matchedBody,
				"timestamp", evt.Info.Timestamp,
				"startupTime", GetStartupTime(),
			)
			return true
		}
		if runCommand(ctx, client, evt, matchedBody) {
			return true
		}
		fields := strings.Fields(matchedBody)
		if len(fields) > 0 {
			cmdName := strings.ToLower(fields[0])
			if clean := strings.TrimRight(cmdName, ",:;!? \t"); clean != "" {
				cmdName = clean
			}
			if isLikelyCommandName(cmdName) {
				if _, handled := HandleUnknownCommand(cctx, matchedPrefix, cmdName); handled {
					return true
				}
			}
		}
		return false
	}

	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) > 0 {
		cmdName := strings.ToLower(fields[0])
		args := fields[1:]
		rawArgs := strings.TrimSpace(strings.TrimPrefix(text, fields[0]))
		if external.DefaultDispatcher.IsInstalled(cmdName) {
			if isStale(evt) {
				logger.Debug("Skipping external command from message sent before bot startup",
					"command", cmdName,
					"timestamp", evt.Info.Timestamp,
					"startupTime", GetStartupTime(),
				)
				return true
			}
			return external.DefaultDispatcher.Dispatch(ctx, client, evt, cmdName, args, rawArgs)
		}
	}

	// 9. Sticker Commands via reply with arguments
	if text != "" && okStore {
		if isStale(evt) {
			logger.Debug("Skipping quoted sticker command from message sent before bot startup",
				"timestamp", evt.Info.Timestamp,
				"startupTime", GetStartupTime(),
			)
			return true
		}
		if handleQuotedStickerCommand(ctx, client, s.SQLStore, evt, text) {
			return true
		}
	}

	// 10. Run registered Fallback Interceptors (AutoAI)
	fallbackInterceptorsMu.RLock()
	fallbackList := make([]interceptorEntry, len(fallbackInterceptors))
	copy(fallbackList, fallbackInterceptors)
	fallbackInterceptorsMu.RUnlock()

	for _, it := range fallbackList {
		if isStale(evt) {
			logger.Debug("Skipping fallback interceptor from message sent before bot startup",
				"timestamp", evt.Info.Timestamp,
				"startupTime", GetStartupTime(),
			)
			return true
		}
		if it.fn(cctx, text) {
			return true
		}
	}

	return false
}

// ResolveSenderMention resolves the triggering sender to an interactive WhatsApp mention tag (@phone)
// and all associated non-AD JIDs (including Phone Number and LID mappings) for ContextInfo.MentionedJID.
func ResolveSenderMention(cctx *Context) (string, []types.JID) {
	if cctx == nil {
		return "@User", nil
	}

	sender := cctx.Sender
	if sender.IsEmpty() && cctx.Evt != nil && !cctx.Evt.Info.Sender.IsEmpty() {
		sender = cctx.Evt.Info.Sender
	}
	if sender.IsEmpty() {
		sender = cctx.Chat
	}
	if sender.IsEmpty() && cctx.Client != nil && cctx.Client.Store != nil && cctx.Client.Store.ID != nil {
		sender = *cctx.Client.Store.ID
	}
	sender = sender.ToNonAD()
	if sender.IsEmpty() || (sender.Server != types.DefaultUserServer && sender.Server != types.HiddenUserServer) {
		return "@User", nil
	}

	ctx := cctx.GetSendContext()
	client := cctx.Client

	pnJID := sender
	var lidJID types.JID

	switch sender.Server {
	case types.HiddenUserServer:
		lidJID = sender
		if client != nil && client.Store != nil && client.Store.LIDs != nil {
			if pn, err := client.Store.LIDs.GetPNForLID(ctx, sender); err == nil && !pn.IsEmpty() {
				pnJID = pn.ToNonAD()
			}
		}
	case types.DefaultUserServer:
		pnJID = sender
		if client != nil && client.Store != nil && client.Store.LIDs != nil {
			if lid, err := client.Store.LIDs.GetLIDForPN(ctx, sender); err == nil && !lid.IsEmpty() {
				lidJID = lid.ToNonAD()
			}
		}
	}

	// In WhatsApp protocol, interactive text mentions require @<phone_number> (the JID User part).
	// WhatsApp clients match this against ContextInfo.MentionedJID to highlight and render the clickable mention.
	tagUser := pnJID.User
	if tagUser == "" {
		tagUser = sender.User
	}
	if tagUser == "" || tagUser == "User" {
		return "@User", nil
	}
	userTag := "@" + tagUser

	var mentions []types.JID
	seen := make(map[string]bool)
	for _, j := range []types.JID{pnJID, lidJID, sender} {
		if !j.IsEmpty() {
			norm := j.ToNonAD()
			key := norm.String()
			if !seen[key] {
				seen[key] = true
				mentions = append(mentions, norm)
			}
		}
	}

	return userTag, mentions
}

// HandleUnknownCommand handles incoming messages where the prefix matched but the command does not exist.
// It finds the closest registered command, formats the suggestion with user mention, and replies.
func HandleUnknownCommand(cctx *Context, prefix, cmdName string) (string, bool) {
	if cctx == nil {
		return "", false
	}
	if isStale(cctx.Evt) {
		logger.Debug("Skipping unknown command from message sent before bot startup",
			"prefix", prefix,
			"cmdName", cmdName,
			"timestamp", cctx.Evt.Info.Timestamp,
			"startupTime", GetStartupTime(),
		)
		return "", true
	}

	sendCtx := cctx.GetSendContext()
	if s, okStore := GetSQLStore(cctx.Client); okStore {
		if !cctx.IsSudo() {
			if val, err := s.GetSetting(sendCtx, "ban:"+cctx.Sender.ToNonAD().String()); err == nil && val == "true" {
				return "", true
			}
			if val, err := s.GetSetting(sendCtx, "ban:"+cctx.Sender.ToNonAD().User); err == nil && val == "true" {
				return "", true
			}
		}
		botMode, _ := s.GetSetting(sendCtx, "mode")
		if botMode == "private" && !cctx.IsSudo() {
			return "", true
		}
	}

	closest := ClosestCommand(cmdName)
	if closest == "" {
		return "", false
	}

	userTag, mentions := ResolveSenderMention(cctx)

	msg := FormatUnknownCommandSuggestion(userTag, prefix, closest)

	logger.Debug("Dispatcher: unknown command with prefix detected, suggesting closest", "prefix", prefix, "input", cmdName, "suggestion", closest, "user", userTag, "mentions", mentions)
	_ = cctx.ReplyWithMentions(msg, mentions)
	return msg, true
}

func runCommand(ctx context.Context, client *whatsmeow.Client, evt *events.Message, cmdLine string) bool {
	runStart := time.Now()
	if isStale(evt) {
		logger.Debug("Skipping command execution from message sent before bot startup",
			"cmdLine", cmdLine,
			"timestamp", evt.Info.Timestamp,
			"startupTime", GetStartupTime(),
		)
		return true
	}

	fields := strings.Fields(cmdLine)
	if len(fields) == 0 {
		return false
	}

	cmdName := strings.ToLower(fields[0])
	args := fields[1:]
	rawArgs := strings.TrimSpace(strings.TrimPrefix(cmdLine, fields[0]))

	cmd, exists := Get(cmdName)
	if !exists {
		if external.DefaultDispatcher.IsInstalled(cmdName) {
			return external.DefaultDispatcher.Dispatch(ctx, client, evt, cmdName, args, rawArgs)
		}
		return false
	}

	cctx := &Context{
		Ctx:     ctx,
		Client:  client,
		Evt:     evt,
		Chat:    evt.Info.Chat,
		Sender:  evt.Info.Sender,
		Command: cmdName,
		Args:    args,
		RawArgs: rawArgs,
	}

	// Permission checks
	if cmd.GroupOnly && !cctx.IsGroup() {
		_ = cctx.Reply("This command can only be used in group chats.")
		return true
	}

	if !cmd.IsPublic && !cctx.IsSudo() {
		_ = cctx.Reply("This command is restricted to the bot owner and authorized users.")
		return true
	}

	tChecks := time.Now()
	if s, okStore := GetSQLStore(client); okStore {
		if !cctx.IsSudo() {
			if rawBanned, _ := s.GetSetting(ctx, "banned_users"); rawBanned != "" {
				senderStr := cctx.Sender.ToNonAD().String()
				senderUser := cctx.Sender.ToNonAD().User
				for banned := range strings.FieldsSeq(rawBanned) {
					if strings.EqualFold(banned, senderStr) || strings.EqualFold(banned, senderUser) {
						return true
					}
					if cctx.Evt != nil && !cctx.Evt.Info.SenderAlt.IsEmpty() {
						if strings.EqualFold(banned, cctx.Evt.Info.SenderAlt.ToNonAD().String()) || strings.EqualFold(banned, cctx.Evt.Info.SenderAlt.ToNonAD().User) {
							return true
						}
					}
					if cctx.Evt != nil && cctx.Evt.Info.PushName != "" && strings.EqualFold(banned, cctx.Evt.Info.PushName) {
						return true
					}
				}
			}
			if val, err := s.GetSetting(ctx, "ban:"+cctx.Sender.ToNonAD().String()); err == nil && val == "true" {
				return true
			}
			if val, err := s.GetSetting(ctx, "ban:"+cctx.Sender.ToNonAD().User); err == nil && val == "true" {
				return true
			}
		}

		botMode, _ := s.GetSetting(ctx, "mode")
		if botMode == "private" && !cctx.IsSudo() {
			return true
		}

		raw, _ := s.GetSetting(ctx, "disabled_commands")
		if raw != "" {
			for disabled := range strings.FieldsSeq(raw) {
				if strings.EqualFold(disabled, cmdName) {
					_ = cctx.Replyf("Command %q is currently disabled.", cmdName)
					return true
				}
			}
		}
	}
	durChecks := time.Since(tChecks)
	if durChecks > 1*time.Millisecond {
		logger.Debug("[PERF] runCommand: permission and ban checks", "cmd", cmdName, "elapsed", durChecks)
	}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				crashPath := util.RecordCrash(r, "command: "+cmdName, "user: "+cctx.Sender.String(), "chat: "+cctx.Chat.String())
				logger.Error("Panic recovered in command handler", "command", cmdName, "panic", r, "crash_log", crashPath)
				_ = cctx.Reply("An unexpected internal error occurred while executing this command.")
			}
		}()

		cmdStart := time.Now()
		logger.Debug("[PERF] Invoking cmd.Handler", "cmd", cmdName, "preHandlerElapsed", time.Since(runStart))
		err := cmd.Handler(cctx)
		logger.Debug("[PERF] cmd.Handler finished", "cmd", cmdName, "duration", time.Since(cmdStart), "err", err)
		if err != nil {
			LogHandlerErrWithContext(cctx, cmdName, err)
			_ = cctx.Replyf("%v", err)
		}
	}()

	return true
}

func activePrefixes(ctx context.Context, client *whatsmeow.Client) []string {
	if client == nil || client.Store == nil || client.Store.Identities == nil {
		return []string{"."}
	}
	s, ok := client.Store.Identities.(interface {
		GetSetting(ctx context.Context, key string) (string, error)
	})
	if !ok {
		return []string{"."}
	}
	raw, err := s.GetSetting(ctx, "prefix")
	if err != nil || raw == "" {
		return []string{"."}
	}
	parts := strings.Fields(raw)
	if len(parts) == 0 {
		return []string{"."}
	}
	var res []string
	for _, p := range parts {
		if strings.EqualFold(p, "none") || strings.EqualFold(p, "empty") {
			res = append(res, "")
		} else {
			res = append(res, p)
		}
	}
	return res
}

func matchesPrefix(text, prefix string) bool {
	if prefix == "" {
		return true
	}
	if strings.HasPrefix(text, prefix) {
		return true
	}
	return false
}

func extractInteractionDisplayText(evt *events.Message) string {
	if evt == nil || evt.Message == nil {
		return ""
	}
	msg := utils.UnwrapMessageProto(evt.Message)
	if msg == nil {
		return ""
	}
	if btnResp := msg.GetButtonsResponseMessage(); btnResp != nil {
		return btnResp.GetSelectedDisplayText()
	}
	if listResp := msg.GetListResponseMessage(); listResp != nil {
		return listResp.GetTitle()
	}
	if templResp := msg.GetTemplateButtonReplyMessage(); templResp != nil {
		return templResp.GetSelectedDisplayText()
	}
	return ""
}

func handleAutoViewOnce(ctx context.Context, client *whatsmeow.Client, s *sqlstore.SQLStore, evt *events.Message) {
	raw, _ := store.GetSetting(ctx, s, "autovv")
	if raw != "on" {
		return
	}
	mode, _ := store.GetSetting(ctx, s, "autovv_mode")
	var targetJID types.JID
	if (mode == "public" || mode == "chat") && !evt.Info.Chat.IsEmpty() {
		targetJID = evt.Info.Chat
	} else if client.Store.ID != nil {
		targetJID = client.Store.ID.ToNonAD()
	}

	if !targetJID.IsEmpty() {
		go func() {
			_ = utils.UnwrapAndSendViewOnceMessage(context.Background(), client, evt.Message, evt.Info.Sender, evt.Info.PushName, targetJID, evt.Info.ID, evt.Info.Chat)
		}()
	}
}

func handleStickerCommand(ctx context.Context, client *whatsmeow.Client, s *sqlstore.SQLStore, evt *events.Message, stk *waE2E.StickerMessage) bool {
	if stk == nil || len(stk.GetFileSHA256()) == 0 {
		return false
	}

	shaHex := hex.EncodeToString(stk.GetFileSHA256())
	rawCmd, err := store.GetStickerCmd(ctx, s, shaHex)
	if err != nil || strings.TrimSpace(rawCmd) == "" {
		return false
	}

	rawCmd = strings.TrimSpace(rawCmd)
	rawCmd = strings.TrimLeft(rawCmd, "./!#$")

	cmdLine := rawCmd

	// Check if the sticker quotes a message providing extra arguments
	ci := stk.GetContextInfo()
	if ci == nil {
		ci = utils.GetContextInfoFromProto(evt.Message)
	}
	if ci != nil && ci.QuotedMessage != nil {
		quotedText := strings.TrimSpace(utils.ExtractTextFromProto(ci.QuotedMessage))
		if quotedText != "" {
			cmdLine = rawCmd + " " + quotedText
		}
	}

	return runCommand(ctx, client, evt, cmdLine)
}

func handleQuotedStickerCommand(ctx context.Context, client *whatsmeow.Client, s *sqlstore.SQLStore, evt *events.Message, replyText string) bool {
	if s == nil || evt == nil || evt.Message == nil {
		return false
	}

	ci := utils.GetContextInfoFromProto(evt.Message)
	if ci == nil || ci.QuotedMessage == nil {
		return false
	}

	quotedMsg := utils.UnwrapMessageProto(ci.QuotedMessage)
	if quotedMsg == nil {
		return false
	}

	stk := quotedMsg.GetStickerMessage()
	if stk == nil || len(stk.GetFileSHA256()) == 0 {
		return false
	}

	shaHex := hex.EncodeToString(stk.GetFileSHA256())
	rawCmd, err := store.GetStickerCmd(ctx, s, shaHex)
	if err != nil || strings.TrimSpace(rawCmd) == "" {
		return false
	}

	rawCmd = strings.TrimSpace(rawCmd)
	rawCmd = strings.TrimLeft(rawCmd, "./!#$")

	replyText = strings.TrimSpace(replyText)
	cmdLine := rawCmd
	if replyText != "" {
		cmdLine = rawCmd + " " + replyText
	}

	return runCommand(ctx, client, evt, cmdLine)
}

func isBotMentioned(client *whatsmeow.Client, evt *events.Message) bool {
	if client == nil || client.Store == nil || client.Store.ID == nil {
		return false
	}
	ourJID := client.Store.ID.ToNonAD()

	var mentions []string
	if ext := evt.Message.GetExtendedTextMessage(); ext != nil {
		if ci := ext.GetContextInfo(); ci != nil {
			mentions = ci.MentionedJID
		}
	}

	ourLID := ourJID
	if !client.Store.LID.IsEmpty() {
		ourLID = client.Store.LID.ToNonAD()
	} else if ourJID.Server == types.DefaultUserServer && client.Store.LIDs != nil {
		if lid, err := client.Store.LIDs.GetLIDForPN(context.Background(), ourJID); err == nil && !lid.IsEmpty() {
			ourLID = lid.ToNonAD()
		}
	} else if ourJID.Server == types.HiddenUserServer && client.Store.LIDs != nil {
		if pn, err := client.Store.LIDs.GetPNForLID(context.Background(), ourJID); err == nil && !pn.IsEmpty() {
			ourLID = pn.ToNonAD()
		}
	}

	for _, m := range mentions {
		mj, err := types.ParseJID(m)
		if err == nil {
			mj = mj.ToNonAD()
			if mj == ourJID || mj == ourLID {
				return true
			}
		}
	}
	return false
}

func handleFiltersAndBGM(ctx context.Context, client *whatsmeow.Client, s *sqlstore.SQLStore, evt *events.Message, text string) bool {
	trigger := strings.TrimSpace(strings.ToLower(text))
	if trigger == "" {
		return false
	}

	type lookupResult struct {
		proto string
		err   error
	}

	bgmChan := make(chan lookupResult, 1)
	filterChan := make(chan lookupResult, 1)

	go func() {
		p, err := store.GetBGM(ctx, s, trigger)
		bgmChan <- lookupResult{proto: p, err: err}
	}()

	go func() {
		p, err := store.GetFilter(ctx, s, trigger)
		filterChan <- lookupResult{proto: p, err: err}
	}()

	bgmRes := <-bgmChan
	if bgmRes.err == nil && bgmRes.proto != "" {
		if msg, err := utils.DecodeProtoMessage(bgmRes.proto); err == nil {
			setReplyContextInfo(msg, evt)
			_, _ = client.SendMessage(ctx, evt.Info.Chat, msg)
			return true
		}
	}

	filterRes := <-filterChan
	if filterRes.err == nil && filterRes.proto != "" {
		if msg, err := utils.DecodeProtoMessage(filterRes.proto); err == nil {
			setReplyContextInfo(msg, evt)
			_, _ = client.SendMessage(ctx, evt.Info.Chat, msg)
			return true
		}
	}

	return false
}

func setReplyContextInfo(msg *waE2E.Message, evt *events.Message) {
	if msg == nil || evt == nil {
		return
	}
	ci := &waE2E.ContextInfo{
		StanzaID:      &evt.Info.ID,
		Participant:   new(string),
		QuotedMessage: evt.Message,
	}
	senderStr := evt.Info.Sender.ToNonAD().String()
	*ci.Participant = senderStr

	if ext := msg.ExtendedTextMessage; ext != nil {
		ext.ContextInfo = ci
	} else if img := msg.ImageMessage; img != nil {
		img.ContextInfo = ci
	} else if vid := msg.VideoMessage; vid != nil {
		vid.ContextInfo = ci
	} else if aud := msg.AudioMessage; aud != nil {
		aud.ContextInfo = ci
	} else if doc := msg.DocumentMessage; doc != nil {
		doc.ContextInfo = ci
	} else if stk := msg.StickerMessage; stk != nil {
		stk.ContextInfo = ci
	}
}
