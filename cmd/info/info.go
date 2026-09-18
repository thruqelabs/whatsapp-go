package info

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"whatsrook"
	"whatsrook/cmd/dispatch"
	"whatsrook/cmd/games"
	"whatsrook/cmd/settings"
	"whatsrook/cmd/tools"
	"whatsrook/cmd/updater"
	"whatsrook/util"
	"whatsrook/util/httpx"
	"whatsrook/util/logger"
	"whatsrook/util/media"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func init() {
	dispatch.Register(&dispatch.Command{
		Name:        "alive",
		Description: "Check bot online status, uptime, system stats, and custom alive template or replied media (image/video/audio)",
		Category:    "info",
		IsPublic:    true,
		Handler:     handleAlive,
	})

	dispatch.Register(&dispatch.Command{
		Name:        "menu",
		Alias:       "help,list,panel",
		Description: "Show all available commands grouped by category",
		Category:    "info",
		IsPublic:    true,
		Handler:     handleMenu,
	})

	dispatch.Register(&dispatch.Command{
		Name:        "ping",
		Description: "Check bot response latency",
		Category:    "info",
		IsPublic:    true,
		Handler:     handlePing,
	})

	dispatch.Register(&dispatch.Command{
		Name:        "repo",
		Alias:       "sc",
		Description: "Show the GitHub repository link and project info",
		Category:    "info",
		IsPublic:    true,
		Handler:     handleRepo,
	})

	dispatch.Register(&dispatch.Command{
		Name:        "uptime",
		Alias:       "runtime",
		Description: "Show how long the bot has been running",
		Category:    "info",
		IsPublic:    true,
		Handler:     handleUptime,
	})
}

func handleAlive(ctx *dispatch.Context) error {
	s, okStore := dispatch.GetStore(ctx)

	quotedMsg := ctx.GetQuotedMessage()
	if quotedMsg != nil && ctx.IsSudo() {
		var mediaData []byte
		var mediaType string
		var mime string
		var errDl error

		if img := quotedMsg.GetImageMessage(); img != nil {
			mediaData, errDl = ctx.Client.Download(ctx.Ctx, img)
			mediaType = "image"
			mime = img.GetMimetype()
		} else if vid := quotedMsg.GetVideoMessage(); vid != nil {
			mediaData, errDl = ctx.Client.Download(ctx.Ctx, vid)
			mediaType = "video"
			mime = vid.GetMimetype()
		} else if aud := quotedMsg.GetAudioMessage(); aud != nil {
			mediaData, errDl = ctx.Client.Download(ctx.Ctx, aud)
			mediaType = "audio"
			mime = aud.GetMimetype()
		}

		if errDl == nil && len(mediaData) > 0 {
			mediaDir := dispatch.GetSessionMediaDir(ctx.Client)
			_ = os.MkdirAll(mediaDir, 0755)
			mediaFile := filepath.Join(mediaDir, "alive_media.bin")
			if errWrite := os.WriteFile(mediaFile, mediaData, 0644); errWrite == nil {
				if okStore {
					_ = s.PutSetting(ctx.Ctx, AliveMediaTypeKey, mediaType)
					_ = s.PutSetting(ctx.Ctx, AliveMediaMimeKey, mime)
					_ = s.PutSetting(ctx.Ctx, AliveMediaFileKey, mediaFile)
				}
				caption := whatsrook.ExtractTextFromProto(quotedMsg)
				if strings.TrimSpace(ctx.RawArgs) != "" && !strings.EqualFold(strings.Fields(ctx.RawArgs)[0], "media") {
					caption = strings.TrimSpace(ctx.RawArgs)
				}
				if caption != "" && okStore {
					_ = s.PutSetting(ctx.Ctx, AliveTemplateKey, caption)
				}

				return ctx.Replyf("Saved replied %s as your alive media message!\n\nImage/Video will include the caption, and Audio will include music card context info.\nUse `%salive` to test it.", mediaType, ctx.GetPrefix())
			}
		}
	}

	args := strings.Fields(ctx.RawArgs)
	if len(args) > 0 {
		sub := strings.ToLower(args[0])

		switch sub {
		case "customize", "help", "guide":
			if len(args) > 1 {
				if !ctx.IsSudo() {
					return ctx.Reply("Only sudoers/owners can customize the alive message template.")
				}
				newTpl := strings.TrimSpace(ctx.RawArgs[len(args[0]):])
				if strings.EqualFold(newTpl, "reset") || strings.EqualFold(newTpl, "clear") || strings.EqualFold(newTpl, "default") {
					if okStore {
						_ = s.PutSetting(ctx.Ctx, AliveTemplateKey, "")
						_ = s.PutSetting(ctx.Ctx, AliveMediaTypeKey, "")
						_ = s.PutSetting(ctx.Ctx, AliveMediaFileKey, "")
					}
					return renderAliveResponse(ctx, DefaultAliveTemplate, "")
				}
				if okStore {
					if err := s.PutSetting(ctx.Ctx, AliveTemplateKey, newTpl); err != nil {
						return ctx.Reply("Failed to save alive template: " + err.Error())
					}
				}
				return renderAliveResponse(ctx, newTpl, "")
			}
			return sendAliveCustomizeGuide(ctx)

		case "msg", "set", "template":
			if !ctx.IsSudo() {
				return ctx.Reply("Only sudoers/owners can customize the alive message template.")
			}
			if len(args) < 2 {
				curr := DefaultAliveTemplate
				if okStore {
					if val, err := s.GetSetting(ctx.Ctx, AliveTemplateKey); err == nil && val != "" {
						curr = val
					}
				}
				return ctx.Reply("Current Alive Message Template:\n\n" + curr)
			}
			newTpl := strings.TrimSpace(ctx.RawArgs[len(args[0]):])
			if strings.EqualFold(newTpl, "reset") || strings.EqualFold(newTpl, "clear") || strings.EqualFold(newTpl, "default") {
				if okStore {
					_ = s.PutSetting(ctx.Ctx, AliveTemplateKey, "")
					_ = s.PutSetting(ctx.Ctx, AliveMediaTypeKey, "")
					_ = s.PutSetting(ctx.Ctx, AliveMediaFileKey, "")
				}
				return renderAliveResponse(ctx, DefaultAliveTemplate, "")
			}
			if okStore {
				if err := s.PutSetting(ctx.Ctx, AliveTemplateKey, newTpl); err != nil {
					return ctx.Reply("Failed to save alive template: " + err.Error())
				}
			}
			return renderAliveResponse(ctx, newTpl, "")

		case "get", "show", "view":
			curr := DefaultAliveTemplate
			if okStore {
				if val, err := s.GetSetting(ctx.Ctx, AliveTemplateKey); err == nil && val != "" {
					curr = val
				}
			}
			return ctx.Reply("Current Alive Message Template:\n\n" + curr)

		case "media":
			if !ctx.IsSudo() {
				return ctx.Reply("Only sudoers/owners can set alive media URL.")
			}
			if len(args) < 2 {
				curr := "none"
				if okStore {
					if val, err := s.GetSetting(ctx.Ctx, AliveMediaKey); err == nil && val != "" {
						curr = val
					}
				}
				return ctx.Reply("Current Alive Media URL: " + curr)
			}
			urlVal := strings.TrimSpace(args[1])
			if strings.EqualFold(urlVal, "clear") || strings.EqualFold(urlVal, "off") || strings.EqualFold(urlVal, "none") {
				if okStore {
					_ = s.PutSetting(ctx.Ctx, AliveMediaKey, "")
					_ = s.PutSetting(ctx.Ctx, AliveMediaTypeKey, "")
					_ = s.PutSetting(ctx.Ctx, AliveMediaFileKey, "")
				}
				return ctx.Reply("Alive media URL cleared.")
			}
			if okStore {
				if err := s.PutSetting(ctx.Ctx, AliveMediaKey, urlVal); err != nil {
					return ctx.Reply("Failed to save alive media: " + err.Error())
				}
			}
			return ctx.Reply("Alive media URL updated successfully!")

		case "reset", "clear":
			if !ctx.IsSudo() {
				return ctx.Reply("Only sudoers/owners can reset alive configuration.")
			}
			if okStore {
				_ = s.PutSetting(ctx.Ctx, AliveTemplateKey, "")
				_ = s.PutSetting(ctx.Ctx, AliveMediaTypeKey, "")
				_ = s.PutSetting(ctx.Ctx, AliveMediaMimeKey, "")
				_ = s.PutSetting(ctx.Ctx, AliveMediaFileKey, "")
			}
			return ctx.Reply("Alive template and custom media reset to default.")
		}
	}

	tpl := DefaultAliveTemplate
	if okStore {
		if val, err := s.GetSetting(ctx.Ctx, AliveTemplateKey); err == nil && val != "" {
			tpl = val
		}
	}

	return renderAliveResponse(ctx, tpl, "")
}

func renderAliveResponse(ctx *dispatch.Context, tpl, fallbackMediaURL string) error {
	startMeasure := time.Now()
	uptime := util.FormatDuration(time.Since(util.BootTime))

	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	ramUsage := dispatch.Sprintf("%.2f MB", float64(memStats.Alloc)/1024/1024)
	goroutines := strconv.Itoa(runtime.NumGoroutine())

	latency := dispatch.Sprintf("%d ms", time.Since(startMeasure).Milliseconds())

	ownerName := "Thruqe"
	if ctx.Client != nil && ctx.Client.Store != nil && ctx.Client.Store.ID != nil {
		ownerName = ctx.Client.Store.ID.ToNonAD().User
	}

	pushName := "User"
	if ctx.Evt != nil && ctx.Evt.Info.PushName != "" {
		pushName = ctx.Evt.Info.PushName
	}

	senderJID := ctx.Sender.ToNonAD()
	userTag := "@" + senderJID.User

	var randomFact, randomQuote, randomJoke, randomRizz string
	if strings.Contains(tpl, "fact") {
		randomFact = games.GetRandomFact(ctx.Ctx)
	}
	if strings.Contains(tpl, "quote") {
		randomQuote = games.GetRandomQuote(ctx.Ctx)
	}
	if strings.Contains(tpl, "joke") {
		randomJoke = games.GetRandomJoke(ctx.Ctx)
	}
	if strings.Contains(tpl, "rizz") {
		randomRizz = games.GetRandomRizz(ctx.Ctx)
	}

	replacer := strings.NewReplacer(
		"{bot}", ctx.GetBotName(),
		"@bot", ctx.GetBotName(),
		"[bot]", ctx.GetBotName(),
		"{owner}", ownerName,
		"@owner", ownerName,
		"[owner]", ownerName,
		"{user}", userTag,
		"@user", userTag,
		"[user]", userTag,
		"{name}", pushName,
		"@name", pushName,
		"[name]", pushName,
		"{uptime}", uptime,
		"@uptime", uptime,
		"[uptime]", uptime,
		"{latency}", latency,
		"@latency", latency,
		"[latency]", latency,
		"{ram}", ramUsage,
		"@ram", ramUsage,
		"[ram]", ramUsage,
		"{goroutines}", goroutines,
		"@goroutines", goroutines,
		"[goroutines]", goroutines,
		"{version}", "v2.5.0",
		"@version", "v2.5.0",
		"[version]", "v2.5.0",
		"{prefix}", ctx.GetPrefix(),
		"@prefix", ctx.GetPrefix(),
		"[prefix]", ctx.GetPrefix(),
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

	s, okStore := dispatch.GetStore(ctx)
	mediaType := ""
	mime := ""
	mediaFile := ""
	mediaURL := fallbackMediaURL

	if okStore {
		mType, _ := s.GetSetting(ctx.Ctx, AliveMediaTypeKey)
		mMime, _ := s.GetSetting(ctx.Ctx, AliveMediaMimeKey)
		mFile, _ := s.GetSetting(ctx.Ctx, AliveMediaFileKey)
		mURL, _ := s.GetSetting(ctx.Ctx, AliveMediaKey)

		if mType != "" && mFile != "" {
			mediaType = mType
			mime = mMime
			mediaFile = mFile
		} else if mURL != "" {
			mediaURL = mURL
		}
	}

	var mentions []types.JID
	if strings.Contains(tpl, "@user") || strings.Contains(tpl, "{user}") || strings.Contains(tpl, "[user]") {
		mentions = []types.JID{ctx.Sender}
	}

	if mediaType != "" && mediaFile != "" {
		mediaBytes, errRead := os.ReadFile(mediaFile)
		if errRead == nil && len(mediaBytes) > 0 {
			switch mediaType {
			case "image":
				if mime == "" {
					mime = "image/jpeg"
				}
				return ctx.ReplyWithImageWithMentions(mediaBytes, mime, bodyText, mentions)
			case "video":
				if mime == "" {
					mime = "video/mp4"
				}
				return ctx.ReplyWithVideoWithMentions(mediaBytes, mime, bodyText, mentions)
			case "audio":
				return replyWithAliveAudioCard(ctx, mediaBytes, bodyText, ownerName)
			}
		}
	}

	if mediaURL != "" {
		imgBytes, errDl := httpx.FetchBytes(ctx.Ctx, mediaURL)
		if errDl == nil && len(imgBytes) > 0 {
			return ctx.ReplyWithImageWithMentions(imgBytes, "image/jpeg", bodyText, mentions)
		}
		bodyText = bodyText + "\n\n" + mediaURL
	}

	if len(mentions) > 0 {
		return ctx.ReplyWithMentions(bodyText, mentions)
	}

	return ctx.Reply(bodyText)
}

func replyWithAliveAudioCard(ctx *dispatch.Context, data []byte, bodyText, ownerName string) error {
	meta, errMeta := media.EnsureOpusPTT(ctx.Ctx, data)
	mimetype := "audio/ogg; codecs=opus"
	if errMeta == nil && meta != nil && meta.Converted && len(meta.Data) > 0 {
		data = meta.Data
	} else {
		mimetype = "audio/mp4"
	}

	uploaded, err := ctx.Client.Upload(ctx.Ctx, data, whatsmeow.MediaAudio)
	if err != nil {
		return errors.New("audio upload failed: " + err.Error())
	}

	botName := ctx.GetBotName()
	title := dispatch.Sprintf("%s IS ALIVE", strings.ToUpper(botName))
	body := "Owner: " + ownerName
	sourceURL := "https://github.com/ThruqeLabs/whatsrook"

	adInfo := &waE2E.ContextInfo_ExternalAdReplyInfo{
		Title:                 new(title),
		Body:                  new(body),
		SourceURL:             new(sourceURL),
		MediaType:             waE2E.ContextInfo_ExternalAdReplyInfo_IMAGE.Enum(),
		RenderLargerThumbnail: new(true),
		ShowAdAttribution:     new(false),
	}

	cinfo := &waE2E.ContextInfo{
		ExternalAdReply: adInfo,
	}

	senderJID := ctx.Sender.ToNonAD()
	if strings.Contains(bodyText, "@user") || strings.Contains(bodyText, "{user}") || strings.Contains(bodyText, "[user]") {
		cinfo.MentionedJID = []string{senderJID.String()}
	}

	fileLength := uint64(len(data))
	msg := &waE2E.Message{
		AudioMessage: &waE2E.AudioMessage{
			URL:           &uploaded.URL,
			DirectPath:    &uploaded.DirectPath,
			MediaKey:      uploaded.MediaKey,
			Mimetype:      new(mimetype),
			FileEncSHA256: uploaded.FileEncSHA256,
			FileSHA256:    uploaded.FileSHA256,
			FileLength:    &fileLength,
			PTT:           new(true),
			ContextInfo:   cinfo,
		},
	}

	if meta != nil {
		if meta.Seconds > 0 {
			msg.AudioMessage.Seconds = new(meta.Seconds)
		}
		if len(meta.Waveform) > 0 {
			msg.AudioMessage.Waveform = meta.Waveform
		}
	}

	_, err = ctx.Client.SendMessage(ctx.Ctx, ctx.Chat, msg)
	return err
}

func sendAliveCustomizeGuide(ctx *dispatch.Context) error {
	p := ctx.GetPrefix()
	return ctx.Text().
		Header("ALIVE CUSTOMIZATION GUIDE").
		Section("Usage").
		Bulletf("Check Alive Status : %salive", p).
		Bulletf("Customize Message  : %salive customize <your template> or %salive msg <template>", p, p).
		Bulletf("Reply with Media   : Reply to an image, video, or audio message with %salive to set it as your alive media!", p).
		Indent(4, "- Image/Video : Sent with your custom text template as the caption.").NewLine().
		Indent(4, "- Audio       : Sent as an audio voice note with a music card (context info).").NewLine().
		Bulletf("Custom Media URL   : %salive media <url | clear>", p).
		Bulletf("Reset to Default   : %salive msg reset", p).
		Blank().
		Section("Available Placeholders").
		Bullet("@user / {user} / [user]     : Sender mention tag").
		Bullet("@name / {name} / [name]     : Sender pushname").
		Bullet("@uptime / {uptime} / [uptime] : Active system uptime").
		Bullet("@bot / {bot} / [bot]       : Bot display name").
		Bullet("@owner / {owner} / [owner]   : Bot owner user ID").
		Bullet("@latency / {latency} / [latency]: Response latency").
		Bullet("@ram / {ram} / [ram]       : Allocated RAM usage").
		Bullet("@goroutines / {goroutines}  : Active Go routines").
		Bullet("@version / {version}       : Engine version").
		Bullet("@prefix / {prefix} / [prefix] : Active command prefix").
		Bullet("@fact / {fact} / [fact]     : Random fact from API").
		Bullet("@quote / {quote} / [quote]   : Random quote from API").
		Bullet("@joke / {joke} / [joke]     : Random joke from API").
		Bullet("@rizz / {rizz} / [rizz]     : Random rizz from API").
		Blank().
		Section("Example Custom Templates").
		Linef("%salive customize @user I am alive and operational. Uptime: @uptime", p).
		Linef("%salive customize Hello @name, @bot is online!", p).
		Reply()
}

func HandlePendingMenuMediaReply(ctx context.Context, client *whatsmeow.Client, evt *events.Message) bool {
	if evt == nil || evt.Message == nil {
		return false
	}

	key := evt.Info.Chat.ToNonAD().String() + ":" + evt.Info.Sender.ToNonAD().String()
	MenuThumbPromptsMu.RLock()
	promptTime, active := PendingMenuThumbPrompts[key]
	MenuThumbPromptsMu.RUnlock()

	if active && time.Since(promptTime) > 5*time.Minute {
		MenuThumbPromptsMu.Lock()
		delete(PendingMenuThumbPrompts, key)
		MenuThumbPromptsMu.Unlock()
		active = false
	}

	if !active {
		return false
	}

	text := whatsrook.ExtractMessageText(evt)
	fakeCtx := &dispatch.Context{
		Ctx:    ctx,
		Client: client,
		Evt:    evt,
		Chat:   evt.Info.Chat,
		Sender: evt.Info.Sender,
	}

	var prefixes []string
	if s, ok := dispatch.GetSQLStore(client); ok {
		if p, err := s.GetSetting(ctx, "prefix"); err == nil && strings.TrimSpace(p) != "" {
			prefixes = strings.Fields(p)
		}
	}
	if len(prefixes) == 0 {
		prefixes = []string{"."}
	}
	if text != "" {
		for _, pref := range prefixes {
			if pref != "" && strings.HasPrefix(text, pref) {
				MenuThumbPromptsMu.Lock()
				delete(PendingMenuThumbPrompts, key)
				MenuThumbPromptsMu.Unlock()
				return false
			}
		}
	}

	downloadable, isVideo, mime := whatsrook.ExtractMediaFromEvent(evt)
	if downloadable == nil {
		return false
	}

	MenuThumbPromptsMu.Lock()
	delete(PendingMenuThumbPrompts, key)
	MenuThumbPromptsMu.Unlock()

	logger.Info("HandlePendingMenuMediaReply: Downloading custom menu media", "chat", key, "mime", mime, "isVideo", isVideo)
	data, err := client.Download(ctx, downloadable)

	if err != nil || len(data) == 0 {
		logger.Error("HandlePendingMenuMediaReply: Download failed", "chat", key, "err", err)
		_ = fakeCtx.Reply("Failed to download media for menu thumbnail.")
		return true
	}

	authDir := settings.GetSessionAuthDir(client)
	targetPath, errProc := settings.ProcessAndSaveThumbnail(ctx, authDir, data, isVideo)
	if errProc != nil {
		_ = fakeCtx.Replyf("Failed to process menu thumbnail: %v", errProc)
		return true
	}

	if s, ok := dispatch.GetSQLStore(client); ok {
		_ = s.PutSetting(ctx, "menu_thumbnail_path", targetPath)
	}

	_ = fakeCtx.Replyf("Bot menu thumbnail updated successfully! Type %smenu to view your custom thumbnail.", fakeCtx.GetPrefix())
	return true
}

func handleMenu(ctx *dispatch.Context) error {
	args := strings.Fields(ctx.RawArgs)
	if len(args) > 0 {
		sub := strings.ToLower(args[0])
		if sub == "reconfigure" || sub == "reconfig" || sub == "wizard" || sub == "setup" {
			return ctx.Reply("To reconfigure the bot interactively, type `" + ctx.GetPrefix() + "setbot`")
		}
		if sub == "customize" || sub == "custom" {
			key := ctx.Chat.ToNonAD().String() + ":" + ctx.Sender.ToNonAD().String()
			MenuThumbPromptsMu.Lock()
			PendingMenuThumbPrompts[key] = time.Now()
			MenuThumbPromptsMu.Unlock()
			return ctx.Reply("Upload or reply with an image (.jpg/.png) or video (.mp4) to set it as the custom bot menu thumbnail.\n\nTo restore default: " + ctx.GetPrefix() + "menu reset")
		}
		if sub == "reset" {
			key := ctx.Chat.ToNonAD().String() + ":" + ctx.Sender.ToNonAD().String()
			MenuThumbPromptsMu.Lock()
			delete(PendingMenuThumbPrompts, key)
			MenuThumbPromptsMu.Unlock()

			authDir := settings.GetSessionAuthDir(ctx.Client)
			if s, ok := dispatch.GetStore(ctx); ok {
				_ = s.PutSetting(ctx.Ctx, "menu_thumbnail_path", "")
			}
			_ = os.Remove(filepath.Join(authDir, "custom_menu_thumbnail.mp4"))
			_ = os.Remove(filepath.Join(authDir, "custom_menu_thumbnail.jpg"))
			return ctx.Reply("Bot menu thumbnail reset to default.")
		}
	}

	type entry struct{ name, desc string }
	categoryOrder := []string{}
	categories := map[string][]entry{}
	seenCat := map[string]bool{}

	hiddenCmds := map[string]bool{
		"menu": true,
	}

	displayedCount := 0
	for _, cmd := range dispatch.All() {
		if hiddenCmds[strings.ToLower(cmd.Name)] || cmd.HideFromMenu {
			continue
		}
		cat := cmd.Category
		if cat == "" {
			cat = "misc"
		}
		if !seenCat[cat] {
			seenCat[cat] = true
			categoryOrder = append(categoryOrder, cat)
		}
		categories[cat] = append(categories[cat], entry{name: cmd.Name, desc: cmd.Description})
		displayedCount++
	}

	uptime := util.FormatDuration(time.Since(StartTime))
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	usedRAM := ms.Alloc
	platform := runtime.GOOS

	user := ctx.Evt.Info.PushName
	if user == "" {
		user = ctx.Sender.User
	}

	botMode := "public"
	s, ok := dispatch.GetStore(ctx)
	if ok {
		if rawMode, err := s.GetSetting(ctx.Ctx, "mode"); err == nil && rawMode != "" {
			botMode = rawMode
		}
	}

	var matchedCat string
	if len(args) > 0 {
		sub := strings.ToLower(args[0])
		for _, cat := range categoryOrder {
			if strings.EqualFold(cat, sub) {
				matchedCat = cat
				break
			}
		}
	}
	if matchedCat != "" {
		categoryOrder = []string{matchedCat}
	}

	introBuilder := ctx.Text().
		Header(ctx.GetBotName()).
		Field("User", user).
		Field("OS", platform).
		Field("Mem", whatsrook.FormatBytes(usedRAM)).
		Field("Plugins", strconv.Itoa(displayedCount)).
		Field("Mode", botMode).
		Field("Uptime", uptime).
		Field("Version", updater.GetAppVersion())

	tb := ctx.Text()
	tb.Line("```\n" + introBuilder.Trimmed() + "\n```")
	tb.Blank()

	for _, cat := range categoryOrder {
		cmds := categories[cat]
		sort.Slice(cmds, func(i, j int) bool {
			return cmds[i].name < cmds[j].name
		})
		tb.Linef(" ╭─❏ %s ❏", tools.ToSmallCaps(cat))
		for _, e := range cmds {
			tb.Linef(" │ %s", tools.ToSmallCaps(e.name))
		}
		tb.Line(" ╰─────────────────")
		tb.Blank()
	}

	menuText := tb.Trimmed()

	authDir := settings.GetSessionAuthDir(ctx.Client)
	mediaPath := ""
	if ok {
		if custom, err := s.GetSetting(ctx.Ctx, "menu_thumbnail_path"); err == nil && custom != "" {
			if _, errStat := os.Stat(custom); errStat == nil {
				mediaPath = custom
			}
		}
	}
	if mediaPath == "" {
		candidates := []string{
			filepath.Join(authDir, "custom_menu_thumbnail.jpg"),
			filepath.Join(authDir, "custom_menu_thumbnail.jpeg"),
			filepath.Join(authDir, "custom_menu_thumbnail.png"),
			filepath.Join(authDir, "custom_menu_thumbnail.webp"),
			filepath.Join(authDir, "custom_menu_thumbnail.mp4"),
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				mediaPath = c
				break
			}
		}
	}

	if mediaPath != "" {
		if mediaData, err := os.ReadFile(mediaPath); err == nil && len(mediaData) > 0 {
			detectedMime := http.DetectContentType(mediaData)
			ext := strings.ToLower(filepath.Ext(mediaPath))

			isImage := strings.HasPrefix(detectedMime, "image/") ||
				ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".webp"

			if isImage {
				mimeType := detectedMime
				if !strings.HasPrefix(mimeType, "image/") {
					switch ext {
					case ".png":
						mimeType = "image/png"
					case ".webp":
						mimeType = "image/webp"
					default:
						mimeType = "image/jpeg"
					}
				}
				if errSend := ctx.ReplyWithImage(mediaData, mimeType, menuText); errSend == nil {
					return nil
				}
			} else {
				mimeType := detectedMime
				if !strings.HasPrefix(mimeType, "video/") {
					mimeType = "video/mp4"
				}
				if errSend := ctx.ReplyWithVideoGif(mediaData, mimeType, menuText); errSend == nil {
					return nil
				}
			}
		}
	}

	return ctx.Reply(menuText)
}

func handlePing(ctx *dispatch.Context) error {
	startTotal := time.Now()
	var incomingLatency time.Duration
	if ctx.Evt != nil {
		incomingLatency = time.Since(ctx.Evt.Info.Timestamp)
	}
	logger.Debug("[PERF] ping command received", "incoming_latency", incomingLatency, "chat", ctx.Chat.String())

	startReply := time.Now()
	msgID, err := ctx.ReplyWithID("Ping...")
	replyDuration := time.Since(startReply)
	if err != nil {
		logger.Error("[PERF] ping ReplyWithID failed", "err", err, "duration", replyDuration)
		return err
	}
	logger.Debug("[PERF] ping ReplyWithID finished", "reply_duration", replyDuration, "msg_id", msgID)

	elapsed := replyDuration
	var respText string
	if ms := elapsed.Milliseconds(); ms > 0 {
		respText = dispatch.Sprintf("%d ms", ms)
	} else if us := elapsed.Microseconds(); us > 0 {
		respText = dispatch.Sprintf("%d μs", us)
	} else {
		respText = dispatch.Sprintf("%d μs", elapsed.Nanoseconds())
	}

	startEdit := time.Now()
	_, editErr := ctx.Edit(msgID, respText)
	editDuration := time.Since(startEdit)
	if editErr != nil {
		logger.Warn("[PERF] ping Edit failed, falling back to Reply", "err", editErr, "duration", editDuration)
		_ = ctx.Reply(respText)
	} else {
		logger.Debug("[PERF] ping Edit finished", "edit_duration", editDuration, "msg_id", msgID)
	}

	logger.Debug("[PERF] ping command completed",
		"incoming_latency", incomingLatency,
		"reply_duration", replyDuration,
		"edit_duration", editDuration,
		"total_duration", time.Since(startTotal),
	)
	return nil
}

func handleRepo(ctx *dispatch.Context) error {
	repoURL := "https://github.com/ThruqeLabs/whatsrook"
	return ctx.Text().
		Header("WhatsRook Repository").
		Line(repoURL).
		Blank().
		Line("Please star the repository if you like the project, it helps support and motivate me.").
		Blank().
		Field("Powered by", ctx.GetBotName()).
		Reply()
}

func handleUptime(ctx *dispatch.Context) error {
	out := util.FormatDuration(time.Since(StartTime))
	return ctx.Reply(out)
}
