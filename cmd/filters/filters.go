package filters

import (
	"context"
	"runtime"
	"strconv"
	"strings"
	"time"

	"whatsrook"
	"whatsrook/cmd/dispatch"
	"whatsrook/cmd/games"
	"whatsrook/cmd/store"
	"whatsrook/cmd/updater"
	"whatsrook/util"
	"whatsrook/util/logger"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func init() {
	dispatch.Register(&dispatch.Command{
		Name:        "filter",
		Description: "Add, delete, or list auto-response filters with custom placeholders. Usage: filter [word] [response text] (or reply to media), filter del [word], filter list",
		Category:    "filters",
		IsPublic:    true,
		Handler:     handleFilter,
	})
	dispatch.Register(&dispatch.Command{
		Name:        "bgm",
		Description: "Add, delete, or list audio auto-responses. Usage: bgm [word] (replying to audio), bgm del [word], bgm list",
		Category:    "filters",
		IsPublic:    true,
		Handler:     handleBGM,
	})
	dispatch.Register(&dispatch.Command{
		Name:        "mention",
		Description: "Configure auto-response when the bot is tagged. Usage: mention [text...], mention add (replying to a message), mention del, mention list",
		Category:    "filters",
		IsPublic:    true,
		Handler:     handleMention,
	})
	dispatch.Register(&dispatch.Command{
		Name:        "filteradd",
		Alias:       "addfilter",
		Description: "Add an auto-response filter for a trigger word. Usage: filteradd [word] [response text] (or reply to a message)",
		Category:    "filters",
		IsPublic:    true,
		Handler:     handleAddFilter,
	})
	dispatch.Register(&dispatch.Command{
		Name:        "filterget",
		Alias:       "getfilter",
		Description: "Get and test the auto-response message for a trigger word. Usage: filterget [word]",
		Category:    "filters",
		IsPublic:    true,
		Handler:     handleGetFilter,
	})
	dispatch.Register(&dispatch.Command{
		Name:        "filters",
		Alias:       "listfilters",
		Description: "List all active auto-response filters. Usage: filters",
		Category:    "filters",
		IsPublic:    true,
		Handler:     handleListFilters,
	})
	dispatch.Register(&dispatch.Command{
		Name:        "filterdel",
		Alias:       "delfilter",
		Description: "Remove an auto-response filter. Usage: filterdel [word]",
		Category:    "filters",
		IsPublic:    true,
		Handler:     handleDelFilter,
	})
}

// RenderFilterTemplate evaluates and replaces all custom placeholders in a template string,
// returning the rendered text along with any extracted mention JID strings.
func RenderFilterTemplate(ctx context.Context, client *whatsmeow.Client, evt *events.Message, tpl string) (string, []string) {
	if tpl == "" {
		return "", nil
	}

	startMeasure := time.Now()
	uptime := util.FormatDuration(time.Since(util.BootTime))

	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	ramUsage := dispatch.Sprintf("%.2f MB", float64(memStats.Alloc)/1024/1024)
	goroutines := strconv.Itoa(runtime.NumGoroutine())

	elapsed := time.Since(startMeasure)
	var latency string
	if ms := elapsed.Milliseconds(); ms > 0 {
		latency = dispatch.Sprintf("%dms", ms)
	} else {
		latency = dispatch.Sprintf("%dns", elapsed.Nanoseconds())
	}

	ownerName := "Thruqe"
	ownerJIDStr := ""
	if client != nil && client.Store != nil && client.Store.ID != nil {
		ownerName = client.Store.ID.ToNonAD().User
		ownerJIDStr = client.Store.ID.ToNonAD().String()
	}

	pushName := "User"
	senderPhone := "User"
	senderJIDStr := ""
	var senderJID types.JID
	var resolvedJID types.JID

	if evt != nil {
		senderJID = evt.Info.Sender.ToNonAD()
		if senderJID.IsEmpty() {
			senderJID = evt.Info.Chat.ToNonAD()
		}
		if evt.Info.PushName != "" {
			pushName = evt.Info.PushName
		}
		senderPhone = senderJID.User
		senderJIDStr = senderJID.String()

		if client != nil {
			rJID, username := whatsrook.ResolveMentionRaw(ctx, client, senderJID)
			resolvedJID = rJID
			if pushName == "User" && username != "" {
				pushName = username
			}
			if resolvedJID.Server == types.DefaultUserServer && resolvedJID.User != "" {
				senderPhone = resolvedJID.User
			}
		}
	}

	userTag := "@" + senderPhone
	if senderPhone == "" || senderPhone == "User" {
		userTag = "@user"
	}

	botName := "WhatsRook"
	prefix := "."
	if s, ok := dispatch.GetSQLStore(client); ok {
		botName = dispatch.GetBotName(ctx, s)
		if p, err := s.GetSetting(ctx, "prefix"); err == nil && strings.TrimSpace(p) != "" {
			prefix = strings.Fields(p)[0]
		}
	}
	version := updater.GetAppVersion()
	if version == "" {
		version = "v2.5.0"
	}

	groupName := "Direct Message"
	if evt != nil {
		if evt.Info.Chat.Server == "g.us" {
			groupName = evt.Info.Chat.String()
			if client != nil {
				if info, err := client.GetGroupInfo(ctx, evt.Info.Chat); err == nil && info != nil && info.Name != "" {
					groupName = info.Name
				}
			}
		} else {
			if pushName != "" && pushName != "User" {
				groupName = pushName
			}
		}
	}

	now := time.Now()
	timeStr := now.Format("03:04:05 PM")
	dateStr := now.Format("02 Jan 2006")
	dayStr := now.Format("Monday")

	lowerTpl := strings.ToLower(tpl)
	var randomFact, randomQuote, randomJoke, randomRizz string
	if strings.Contains(lowerTpl, "fact") {
		randomFact = games.GetRandomFact(ctx)
	}
	if strings.Contains(lowerTpl, "quote") {
		randomQuote = games.GetRandomQuote(ctx)
	}
	if strings.Contains(lowerTpl, "joke") {
		randomJoke = games.GetRandomJoke(ctx)
	}
	if strings.Contains(lowerTpl, "rizz") {
		randomRizz = games.GetRandomRizz(ctx)
	}

	replacer := strings.NewReplacer(
		"{bot}", botName,
		"@bot", botName,
		"[bot]", botName,
		"{owner}", ownerName,
		"@owner", ownerName,
		"[owner]", ownerName,
		"{owner_jid}", ownerJIDStr,
		"@owner_jid", ownerJIDStr,
		"[owner_jid]", ownerJIDStr,
		"{user}", userTag,
		"@user", userTag,
		"[user]", userTag,
		"{name}", pushName,
		"@name", pushName,
		"[name]", pushName,
		"{phone}", senderPhone,
		"@phone", senderPhone,
		"[phone]", senderPhone,
		"{user_id}", senderPhone,
		"@user_id", senderPhone,
		"[user_id]", senderPhone,
		"{user_jid}", senderJIDStr,
		"@user_jid", senderJIDStr,
		"[user_jid]", senderJIDStr,
		"{jid}", senderJIDStr,
		"@jid", senderJIDStr,
		"[jid]", senderJIDStr,
		"{uptime}", uptime,
		"@uptime", uptime,
		"[uptime]", uptime,
		"{latency}", latency,
		"@latency", latency,
		"[latency]", latency,
		"{ping}", latency,
		"@ping", latency,
		"[ping]", latency,
		"{ram}", ramUsage,
		"@ram", ramUsage,
		"[ram]", ramUsage,
		"{goroutines}", goroutines,
		"@goroutines", goroutines,
		"[goroutines]", goroutines,
		"{version}", version,
		"@version", version,
		"[version]", version,
		"{prefix}", prefix,
		"@prefix", prefix,
		"[prefix]", prefix,
		"{group}", groupName,
		"@group", groupName,
		"[group]", groupName,
		"{chat}", groupName,
		"@chat", groupName,
		"[chat]", groupName,
		"{time}", timeStr,
		"@time", timeStr,
		"[time]", timeStr,
		"{date}", dateStr,
		"@date", dateStr,
		"[date]", dateStr,
		"{day}", dayStr,
		"@day", dayStr,
		"[day]", dayStr,
		"{fact}", randomFact,
		"@fact", randomFact,
		"[fact]", randomFact,
		"{quote}", randomQuote,
		"@quote", randomQuote,
		"[quote]", randomQuote,
		"{joke}", randomJoke,
		"@joke", randomJoke,
		"[joke]", randomJoke,
		"{rizz}", randomRizz,
		"@rizz", randomRizz,
		"[rizz]", randomRizz,
	)

	bodyText := replacer.Replace(tpl)

	var mentions []string
	if strings.Contains(lowerTpl, "user") {
		if !senderJID.IsEmpty() {
			mentions = append(mentions, senderJID.String())
			if !resolvedJID.IsEmpty() && resolvedJID != senderJID {
				mentions = append(mentions, resolvedJID.String())
			}
		}
	}
	if strings.Contains(lowerTpl, "owner") && ownerJIDStr != "" {
		mentions = append(mentions, ownerJIDStr)
	}

	return bodyText, mentions
}

// ApplyFilterPlaceholders evaluates and replaces all custom placeholders in filter response messages.
func ApplyFilterPlaceholders(ctx context.Context, client *whatsmeow.Client, evt *events.Message, msg *waE2E.Message) {
	if msg == nil {
		return
	}

	replaceAndCollect := func(textPtr **string, ciTarget **waE2E.ContextInfo) {
		if textPtr == nil || *textPtr == nil || **textPtr == "" {
			return
		}
		rendered, mentions := RenderFilterTemplate(ctx, client, evt, **textPtr)
		**textPtr = rendered
		if len(mentions) > 0 {
			if *ciTarget == nil {
				*ciTarget = &waE2E.ContextInfo{}
			}
			(*ciTarget).MentionedJID = appendUniqueMentions((*ciTarget).MentionedJID, mentions)
		}
	}

	if msg.Conversation != nil {
		rendered, mentions := RenderFilterTemplate(ctx, client, evt, *msg.Conversation)
		if len(mentions) > 0 {
			text := rendered
			msg.Conversation = nil
			msg.ExtendedTextMessage = &waE2E.ExtendedTextMessage{
				Text: &text,
				ContextInfo: &waE2E.ContextInfo{
					MentionedJID: mentions,
				},
			}
		} else {
			*msg.Conversation = rendered
		}
	}

	if msg.ExtendedTextMessage != nil {
		replaceAndCollect(&msg.ExtendedTextMessage.Text, &msg.ExtendedTextMessage.ContextInfo)
	}
	if msg.ImageMessage != nil {
		replaceAndCollect(&msg.ImageMessage.Caption, &msg.ImageMessage.ContextInfo)
	}
	if msg.VideoMessage != nil {
		replaceAndCollect(&msg.VideoMessage.Caption, &msg.VideoMessage.ContextInfo)
	}
	if msg.DocumentMessage != nil {
		replaceAndCollect(&msg.DocumentMessage.Caption, &msg.DocumentMessage.ContextInfo)
	}
	if msg.InteractiveMessage != nil {
		if msg.InteractiveMessage.Body != nil {
			replaceAndCollect(&msg.InteractiveMessage.Body.Text, &msg.InteractiveMessage.ContextInfo)
		}
		if msg.InteractiveMessage.Footer != nil {
			replaceAndCollect(&msg.InteractiveMessage.Footer.Text, &msg.InteractiveMessage.ContextInfo)
		}
		if msg.InteractiveMessage.Header != nil {
			if msg.InteractiveMessage.Header.Subtitle != nil {
				replaceAndCollect(&msg.InteractiveMessage.Header.Subtitle, &msg.InteractiveMessage.ContextInfo)
			}
			if msg.InteractiveMessage.Header.Title != nil {
				replaceAndCollect(&msg.InteractiveMessage.Header.Title, &msg.InteractiveMessage.ContextInfo)
			}
		}
	}
	if msg.TemplateMessage != nil && msg.TemplateMessage.HydratedTemplate != nil {
		if msg.TemplateMessage.HydratedTemplate.HydratedContentText != nil {
			replaceAndCollect(&msg.TemplateMessage.HydratedTemplate.HydratedContentText, &msg.TemplateMessage.ContextInfo)
		}
		if msg.TemplateMessage.HydratedTemplate.HydratedFooterText != nil {
			replaceAndCollect(&msg.TemplateMessage.HydratedTemplate.HydratedFooterText, &msg.TemplateMessage.ContextInfo)
		}
	}
}

func appendUniqueMentions(existing []string, newMentions []string) []string {
	seen := make(map[string]struct{}, len(existing)+len(newMentions))
	var result []string
	for _, m := range existing {
		if m != "" {
			if _, ok := seen[m]; !ok {
				seen[m] = struct{}{}
				result = append(result, m)
			}
		}
	}
	for _, m := range newMentions {
		if m != "" {
			if _, ok := seen[m]; !ok {
				seen[m] = struct{}{}
				result = append(result, m)
			}
		}
	}
	return result
}

func sendFilterGuide(ctx *dispatch.Context) error {
	p := ctx.GetPrefix()
	return ctx.Text().
		Header("AUTO-RESPONSE FILTERS GUIDE").
		Line("Auto-response filters allow the bot to automatically reply with customized text, images, videos, audio, or documents whenever a specific trigger word is sent.").
		Blank().
		Section("Commands").
		Bulletf("`%sfilter <word> <response>`   : Add a text auto-response filter", p).
		Bulletf("`%sfilter <word>` (replying)  : Add replied media / message as filter", p).
		Bulletf("`%sfilteradd <word> <response>`: Alternative command to add filter", p).
		Bulletf("`%sfilterget <word>`          : Test / retrieve filter response", p).
		Bulletf("`%sfilterdel <word>`          : Delete an existing filter", p).
		Bulletf("`%sfilters`                   : List all active filters", p).
		Blank().
		Section("Supported Custom Placeholders").
		Bullet("@user / {user} / [user]       : Sender mention tag").
		Bullet("@name / {name} / [name]       : Sender push name / username").
		Bullet("@phone / {phone} / [phone]   : Sender phone number").
		Bullet("@jid / {jid} / [jid]         : Sender JID").
		Bullet("@bot / {bot} / [bot]         : Bot display name").
		Bullet("@owner / {owner} / [owner]   : Bot owner name").
		Bullet("@uptime / {uptime} / [uptime]: Active system uptime").
		Bullet("@latency / {latency} / @ping : Response latency").
		Bullet("@ram / {ram} / [ram]         : Allocated RAM usage").
		Bullet("@goroutines / {goroutines}   : Active Go routines").
		Bullet("@version / {version}         : Engine version").
		Bullet("@prefix / {prefix}           : Active command prefix").
		Bullet("@group / {group} / @chat     : Group / Chat name").
		Bullet("@time / {time}               : Current local time").
		Bullet("@date / {date}               : Current date").
		Bullet("@day / {day}                 : Day of the week").
		Bullet("@fact / {fact}               : Random fun fact").
		Bullet("@quote / {quote}             : Random quote").
		Bullet("@joke / {joke}               : Random joke").
		Bullet("@rizz / {rizz}               : Random pickup line / rizz").
		Blank().
		Section("Examples").
		Linef("`%sfilter hi Hello @user! Welcome. I am @bot, active for @uptime.`", p).
		Linef("`%sfilter quote @user, here is your quote of the day: @quote`", p).
		Linef("`%sfilter time Hey @name, current time is @time (@day).`", p).
		Reply()
}

func sendMentionGuide(ctx *dispatch.Context) error {
	p := ctx.GetPrefix()
	return ctx.Text().
		Header("TAG AUTO-RESPONSE GUIDE").
		Line("Configure an automated response when someone tags/mentions the bot in a chat.").
		Blank().
		Section("Commands").
		Bulletf("`%smention <text...>`      : Set text response with placeholders", p).
		Bulletf("`%smention add` (replying) : Set replied media/message as tag response", p).
		Bulletf("`%smention list`           : Show/test current tag response", p).
		Bulletf("`%smention del`            : Remove tag auto-response", p).
		Blank().
		Section("Supported Custom Placeholders").
		Bullet("@user, @name, @phone, @bot, @owner, @uptime, @latency, @ram, @time, @date, @day, @quote, @fact, @joke, @rizz").
		Blank().
		Section("Example").
		Linef("`%smention Hey @user, why are you tagging me? I am busy! Uptime: @uptime`", p).
		Reply()
}

func sendBGMGuide(ctx *dispatch.Context) error {
	p := ctx.GetPrefix()
	return ctx.Text().
		Header("BGM AUTO-RESPONSE GUIDE").
		Line("Configure audio/voice note auto-responses for specific trigger words.").
		Blank().
		Section("Commands").
		Bulletf("`%sbgm <word>` (replying to audio) : Set replied audio as BGM for word", p).
		Bulletf("`%sbgm del <word>`                 : Remove BGM for word", p).
		Bulletf("`%sbgm list`                       : List all active BGMs", p).
		Reply()
}

func handleFilter(ctx *dispatch.Context) error {
	s, ok := dispatch.GetStore(ctx)
	if !ok {
		return ctx.Reply("Settings store unavailable.")
	}
	db := s.GetDB()
	if db == nil {
		return ctx.Reply("Database unavailable.")
	}

	ourJID := ctx.Client.Store.ID.ToNonAD().String()

	if len(ctx.Args) == 0 {
		return sendFilterGuide(ctx)
	}

	action := strings.ToLower(ctx.Args[0])
	switch action {
	case "help", "guide", "placeholders", "vars":
		return sendFilterGuide(ctx)

	case "add", "+":
		if len(ctx.Args) < 2 {
			return ctx.Replyf("Please specify the trigger word.\nUsage: `%sfilter add <word> <response>` (or reply to a message)", ctx.GetPrefix())
		}
		trigger := strings.ToLower(ctx.Args[1])
		var responseProtoMsg *waE2E.Message
		quoted := ctx.GetQuotedMessage()

		if quoted != nil {
			responseProtoMsg = quoted
		} else {
			if len(ctx.Args) < 3 {
				return ctx.Reply("Please reply to a message or provide response text.")
			}
			textVal := strings.Join(ctx.Args[2:], " ")
			responseProtoMsg = &waE2E.Message{
				Conversation: &textVal,
			}
		}

		encoded, err := whatsrook.EncodeProtoMessage(responseProtoMsg)
		if err != nil {
			return ctx.Replyf("Failed to encode filter message: %v", err)
		}

		if err := store.PutFilter(ctx.Ctx, s.SQLStore, trigger, encoded); err != nil {
			return ctx.Reply("Failed to save filter: " + err.Error())
		}

		logger.Debug("handleFilter: filter added", "trigger", trigger, "our_jid", ourJID)
		return ctx.Replyf("Filter added for word %q. Placeholders like `@user`, `@uptime`, `@bot`, `@time`, etc. will be rendered dynamically.", trigger)

	case "del", "delete", "remove", "rm", "-":
		if len(ctx.Args) < 2 {
			return ctx.Replyf("Please specify the trigger word to remove.\nUsage: `%sfilter del <word>`", ctx.GetPrefix())
		}
		trigger := strings.ToLower(ctx.Args[1])

		if err := store.DeleteFilter(ctx.Ctx, s.SQLStore, trigger); err != nil {
			return ctx.Reply("Failed to delete filter: " + err.Error())
		}
		logger.Debug("handleFilter: filter removed", "trigger", trigger, "our_jid", ourJID)
		return ctx.Replyf("Filter for word %q removed.", trigger)

	case "list", "show", "all":
		return handleListFilters(ctx)

	case "get", "test":
		if len(ctx.Args) < 2 {
			return ctx.Replyf("Please specify the trigger word to test.\nUsage: `%sfilter get <word>`", ctx.GetPrefix())
		}
		ctx.Args = ctx.Args[1:]
		return handleGetFilter(ctx)

	default:
		trigger := strings.ToLower(ctx.Args[0])
		var responseProtoMsg *waE2E.Message
		quoted := ctx.GetQuotedMessage()

		if quoted != nil {
			responseProtoMsg = quoted
		} else {
			if len(ctx.Args) < 2 {
				return ctx.Replyf("Please specify response text or reply to a message.\nExample: `%sfilter %s Hello @user! Today is @day.`", ctx.GetPrefix(), trigger)
			}
			textVal := strings.Join(ctx.Args[1:], " ")
			responseProtoMsg = &waE2E.Message{
				Conversation: &textVal,
			}
		}

		encoded, err := whatsrook.EncodeProtoMessage(responseProtoMsg)
		if err != nil {
			return ctx.Replyf("Failed to encode filter message: %v", err)
		}

		if err := store.PutFilter(ctx.Ctx, s.SQLStore, trigger, encoded); err != nil {
			return ctx.Reply("Failed to save filter: " + err.Error())
		}

		logger.Debug("handleFilter: filter added via shorthand", "trigger", trigger, "our_jid", ourJID)
		return ctx.Replyf("Filter added for word %q. Placeholders like `@user`, `@uptime`, `@bot`, `@time`, etc. will be rendered dynamically.", trigger)
	}
}

func handleBGM(ctx *dispatch.Context) error {
	s, ok := dispatch.GetStore(ctx)
	if !ok {
		return ctx.Reply("Settings store unavailable.")
	}

	ourJID := ctx.Client.Store.ID.ToNonAD().String()

	if len(ctx.Args) == 0 {
		return sendBGMGuide(ctx)
	}

	var trigger string
	var responseProtoMsg *waE2E.Message
	quoted := ctx.GetQuotedMessage()

	action := strings.ToLower(ctx.Args[0])
	switch action {
	case "help", "guide":
		return sendBGMGuide(ctx)

	case "add", "+":
		if len(ctx.Args) < 2 {
			return ctx.Replyf("Please specify the trigger word.\nUsage: `%sbgm add <word>` (replying to audio)", ctx.GetPrefix())
		}
		trigger = strings.ToLower(ctx.Args[1])

		if quoted == nil {
			return ctx.Reply("Please reply to the audio message you want to set as the BGM.")
		}
		if quoted.AudioMessage == nil {
			return ctx.Reply("The replied message must be an audio/voice note.")
		}
		responseProtoMsg = quoted

	case "del", "remove", "delete", "rm", "-":
		if len(ctx.Args) < 2 {
			return ctx.Replyf("Please specify the trigger word to remove.\nUsage: `%sbgm del <word>`", ctx.GetPrefix())
		}
		trigger = strings.ToLower(ctx.Args[1])

		if err := store.DeleteBGM(ctx.Ctx, s.SQLStore, trigger); err != nil {
			return ctx.Reply("Failed to delete BGM: " + err.Error())
		}
		logger.Debug("handleBGM: BGM removed", "trigger", trigger, "our_jid", ourJID)
		return ctx.Replyf("BGM for word %q removed.", trigger)

	case "list", "show", "all":
		triggers, err := store.ListBGMs(ctx.Ctx, s.SQLStore)
		if err != nil {
			return ctx.Reply("Failed to query BGMs: " + err.Error())
		}

		if len(triggers) == 0 {
			return ctx.Reply("No BGMs configured.")
		}
		p := ctx.GetPrefix()
		tb := ctx.Text().
			Header("ACTIVE BGMS").
			Fieldf("Total BGMs", "%d", len(triggers)).
			Blank()

		for _, t := range triggers {
			tb.Bulletf("`%s`", t)
		}
		tb.Blank().
			Linef("Delete BGM: `%sbgm del <word>`", p)
		return tb.Reply()

	default:
		trigger = strings.ToLower(ctx.Args[0])

		if quoted == nil {
			return ctx.Reply("Please reply to the audio message you want to set as the BGM.")
		}
		if quoted.AudioMessage == nil {
			return ctx.Reply("The replied message must be an audio/voice note.")
		}
		responseProtoMsg = quoted
	}

	if responseProtoMsg != nil {
		encoded, err := whatsrook.EncodeProtoMessage(responseProtoMsg)
		if err != nil {
			return ctx.Replyf("Failed to encode BGM message: %v", err)
		}

		if err := store.PutBGM(ctx.Ctx, s.SQLStore, trigger, encoded); err != nil {
			return ctx.Reply("Failed to save BGM: " + err.Error())
		}

		logger.Debug("handleBGM: BGM added", "trigger", trigger, "our_jid", ourJID)
		return ctx.Replyf("BGM added for word %q.", trigger)
	}

	return nil
}

func handleMention(ctx *dispatch.Context) error {
	s, ok := dispatch.GetStore(ctx)
	if !ok {
		return ctx.Reply("Settings store unavailable.")
	}

	ourJID := ctx.Client.Store.ID.ToNonAD().String()

	if len(ctx.Args) == 0 {
		return sendMentionGuide(ctx)
	}

	action := strings.ToLower(ctx.Args[0])
	switch action {
	case "help", "guide":
		return sendMentionGuide(ctx)

	case "add", "+":
		quoted := ctx.GetQuotedMessage()
		if quoted == nil {
			if len(ctx.Args) < 2 {
				return ctx.Replyf("Please reply to a message or specify response text.\nExample: `%smention add Hey @user, why did you tag me?`", ctx.GetPrefix())
			}
			textVal := strings.Join(ctx.Args[1:], " ")
			quoted = &waE2E.Message{
				Conversation: &textVal,
			}
		}

		encoded, err := whatsrook.EncodeProtoMessage(quoted)
		if err != nil {
			return ctx.Replyf("Failed to encode mention message: %v", err)
		}

		if err := store.PutSetting(ctx.Ctx, s.SQLStore, "mention_proto", encoded); err != nil {
			return ctx.Reply("Failed to save mention setting: " + err.Error())
		}

		logger.Debug("handleMention: tag auto-response updated", "our_jid", ourJID)
		return ctx.Reply("Tag auto-response configured with placeholder support.")

	case "del", "remove", "clear", "-":
		if err := store.DeleteSetting(ctx.Ctx, s.SQLStore, "mention_proto"); err != nil {
			return ctx.Reply("Failed to delete mention setting: " + err.Error())
		}
		logger.Debug("handleMention: tag auto-response removed", "our_jid", ourJID)
		return ctx.Reply("Tag auto-response removed.")

	case "list", "show", "get", "test":
		mentionProto, err := store.GetSetting(ctx.Ctx, s.SQLStore, "mention_proto")
		if err != nil || mentionProto == "" {
			return ctx.Reply("No tag auto-response configured.")
		}
		if msg, err := whatsrook.DecodeProtoMessage(mentionProto); err == nil {
			ApplyFilterPlaceholders(ctx.Ctx, ctx.Client, ctx.Evt, msg)
			_, _ = ctx.Client.SendMessage(ctx.Ctx, ctx.Chat, msg)
			return nil
		}
		return ctx.Reply("Tag auto-response is currently configured.")

	default:
		textVal := strings.Join(ctx.Args, " ")
		quoted := &waE2E.Message{
			Conversation: &textVal,
		}

		encoded, err := whatsrook.EncodeProtoMessage(quoted)
		if err != nil {
			return ctx.Replyf("Failed to encode mention message: %v", err)
		}

		if err := store.PutSetting(ctx.Ctx, s.SQLStore, "mention_proto", encoded); err != nil {
			return ctx.Reply("Failed to save mention setting: " + err.Error())
		}

		logger.Debug("handleMention: tag auto-response updated via shorthand", "our_jid", ourJID)
		return ctx.Reply("Tag auto-response configured with placeholder support.")
	}
}

func handleAddFilter(ctx *dispatch.Context) error {
	if len(ctx.Args) == 0 {
		p := ctx.GetPrefix()
		return ctx.Replyf("Usage: `%saddfilter <word> <response text>` (or reply to a message)", p)
	}
	return handleFilter(ctx)
}

func handleGetFilter(ctx *dispatch.Context) error {
	s, ok := dispatch.GetStore(ctx)
	if !ok {
		return ctx.Reply("Settings store unavailable.")
	}

	if len(ctx.Args) == 0 {
		return ctx.Replyf("Usage: `%sgetfilter <word>`", ctx.GetPrefix())
	}

	trigger := strings.ToLower(ctx.Args[0])

	filterProto, err := store.GetFilter(ctx.Ctx, s.SQLStore, trigger)
	if err != nil || filterProto == "" {
		return ctx.Replyf("Filter for word %q not found.", trigger)
	}

	msg, err := whatsrook.DecodeProtoMessage(filterProto)
	if err != nil {
		return ctx.Replyf("Failed to decode filter: %v", err)
	}

	ApplyFilterPlaceholders(ctx.Ctx, ctx.Client, ctx.Evt, msg)
	_, err = ctx.Client.SendMessage(ctx.Ctx, ctx.Chat, msg)
	return err
}

func handleListFilters(ctx *dispatch.Context) error {
	s, ok := dispatch.GetStore(ctx)
	if !ok {
		return ctx.Reply("Settings store unavailable.")
	}

	triggers, err := store.ListFilters(ctx.Ctx, s.SQLStore)
	if err != nil {
		return ctx.Reply("Failed to query filters: " + err.Error())
	}

	p := ctx.GetPrefix()
	if len(triggers) == 0 {
		return ctx.Text().
			Header("AUTO-RESPONSE FILTERS").
			Line("No filters configured yet.").
			Blank().
			Linef("Use `%sfilter <word> <response>` to add your first filter.", p).
			Reply()
	}

	tb := ctx.Text().
		Header("ACTIVE FILTERS").
		Fieldf("Total Filters", "%d", len(triggers)).
		Blank()

	for _, t := range triggers {
		tb.Bulletf("`%s`", t)
	}

	tb.Blank().
		Linef("Test filter  : `%sfilterget <word>`", p).
		Linef("Delete filter: `%sfilterdel <word>`", p).
		Linef("Help & placeholders: `%sfilter help`", p)

	return tb.Reply()
}

func handleDelFilter(ctx *dispatch.Context) error {
	s, ok := dispatch.GetStore(ctx)
	if !ok {
		return ctx.Reply("Settings store unavailable.")
	}
	ourJID := ctx.Client.Store.ID.ToNonAD().String()

	if len(ctx.Args) == 0 {
		return ctx.Replyf("Usage: `%sdelfilter <word>`", ctx.GetPrefix())
	}

	trigger := strings.ToLower(ctx.Args[0])

	if err := store.DeleteFilter(ctx.Ctx, s.SQLStore, trigger); err != nil {
		return ctx.Reply("Failed to delete filter: " + err.Error())
	}
	logger.Debug("handleDelFilter: filter removed", "trigger", trigger, "our_jid", ourJID)
	return ctx.Replyf("Filter for word %q removed.", trigger)
}
