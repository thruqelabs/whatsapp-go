package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"whatsrook/util"
	"whatsrook/util/logger"

	"whatsrook"
	"whatsrook/cmd/calls"
	"whatsrook/cmd/dispatch"
	"whatsrook/cmd/group"
	"whatsrook/cmd/info"
	"whatsrook/cmd/settings"
	"whatsrook/cmd/store"
	"whatsrook/cmd/updater"
	"whatsrook/util/qr"

	_ "whatsrook/cmd/ai"
	_ "whatsrook/cmd/business"
	_ "whatsrook/cmd/chats"
	_ "whatsrook/cmd/extensions"
	_ "whatsrook/cmd/filters"
	_ "whatsrook/cmd/games"
	_ "whatsrook/cmd/owner"
	_ "whatsrook/cmd/tools"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// BotConfig encapsulates runtime configuration parameters parsed from the CLI interface.
type BotConfig struct {
	Session         string
	Pair            bool
	QRCode          bool
	Logout          bool
	ClientType      whatsrook.ClientType
	Business        bool
	Database        string
	Verbose         bool
	WSPort          int
	AsyncMessageAck bool
}

// Bot orchestrates the core WhatsApp client, event dispatcher, and API/WebSocket lifecycle.
type Bot struct {
	cfg          BotConfig
	client       *whatsrook.Client
	groupManager *group.GroupManager
	hub          *Hub
	httpServer   *http.Server
	listener     net.Listener
	startupTime  time.Time
	loggedOut    atomic.Bool
	onLoggedOut  func()
	mu           sync.Mutex
}

// NewBot constructs and initializes a new Bot lifecycle manager.
func NewBot(cfg BotConfig) *Bot {
	b := &Bot{
		cfg:          cfg,
		groupManager: group.NewGroupManager(),
		startupTime:  time.Now(),
	}
	dispatch.SetStartupTime(b.startupTime)
	return b
}

// Start boots the WhatsApp client, initializes the WebSocket API server, and enters the event loop.
func (b *Bot) Start(ctx context.Context) error {
	if b.cfg.Session == "" {
		return errors.New("session phone number is required")
	}

	client := whatsrook.NewClient(whatsrook.Config{
		Session:         b.cfg.Session,
		DataDir:         whatsrook.DefaultDataDir(),
		Database:        b.cfg.Database,
		ClientType:      b.cfg.ClientType,
		Business:        b.cfg.Business,
		Verbose:         b.cfg.Verbose,
		AsyncMessageAck: b.cfg.AsyncMessageAck,
	})

	b.mu.Lock()
	b.client = client
	b.mu.Unlock()

	hub := newHub()
	b.mu.Lock()
	b.hub = hub
	b.mu.Unlock()

	unsubLog := logger.AddHook(func(entry logger.LogEntry) {
		hub.Broadcast(EventMessage{
			Kind:    EventLog,
			Payload: entry,
		})
	})
	defer unsubLog()

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", hub.ServeWS(false))

	// Bind to port :0 to allow the OS to allocate a dynamic ephemeral port
	bindAddr := ":0"
	if b.cfg.WSPort > 0 {
		bindAddr = fmt.Sprintf(":%d", b.cfg.WSPort)
	}

	listener, err := net.Listen("tcp", bindAddr)
	if err != nil {
		return fmt.Errorf("failed to bind API listener on %s: %w", bindAddr, err)
	}

	boundPort := listener.Addr().(*net.TCPAddr).Port
	b.listener = listener

	server := &http.Server{Handler: mux}
	b.httpServer = server

	go func() {
		logger.Info("API and WebSocket server online", "port", boundPort, "session", b.cfg.Session, "addr", listener.Addr().String())
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server runtime error", "err", err)
		}
	}()

	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		if listener != nil {
			_ = listener.Close()
		}
	}()

	for {
		err := b.runSession(ctx)

		if err == nil || errors.Is(err, context.Canceled) {
			return nil
		}

		if errors.Is(err, whatsrook.ErrLoggedOut) || strings.Contains(err.Error(), "logged out") || b.loggedOut.Load() {
			logger.Warn("logged out session detected; device record cleared from database")
			b.loggedOut.Store(false)
			return whatsrook.ErrLoggedOut
		}

		if errors.Is(err, whatsrook.ErrPairTimeout) {
			logger.Error("session error", "err", "pairing timed out due to invalid remote response")
			logger.Warn("session action", "warn", "clearing device record and regenerating pairing key")

			b.mu.Lock()
			cli := b.client
			b.mu.Unlock()
			if cli != nil {
				cli.ClearSessionDB(ctx, "")
			}

			for i := 10; i > 0; i-- {
				fmt.Printf("\r  Retrying in %2ds…", i)
				select {
				case <-time.After(time.Second):
				case <-ctx.Done():
					fmt.Println()
					return nil
				}
			}
			fmt.Println("\r  Retrying now…         ")
			continue
		}

		return fmt.Errorf("session encountered unrecoverable error: %w", err)
	}
}

func (b *Bot) runSession(ctx context.Context) error {
	sessionCtx, sessionCancel := context.WithCancel(ctx)
	defer sessionCancel()

	b.mu.Lock()
	b.onLoggedOut = func() {
		sessionCancel()
	}
	b.mu.Unlock()

	defer func() {
		b.mu.Lock()
		b.onLoggedOut = nil
		b.mu.Unlock()
		group.StopAutoMuteScheduler()
		settings.StopAutoBioScheduler()
		_ = b.client.Close()
	}()

	if err := b.client.InitSession(sessionCtx); err != nil {
		return err
	}

	cli := b.client.WAClient()
	if cli == nil {
		return errors.New("failed to initialize lib client")
	}

	if s, ok := cli.Store.Identities.(*sqlstore.SQLStore); ok && s != nil {
		store.InitTables(sessionCtx, s)
		if val, err := store.GetSetting(sessionCtx, s, settings.BotNamePromptDismissedKey); err == nil && val == "true" {
			settings.BotNamePromptDismissedCacheMu.Lock()
			settings.BotNamePromptDismissedCache[s.JID] = true
			if cli.Store != nil && cli.Store.ID != nil {
				settings.BotNamePromptDismissedCache[cli.Store.ID.ToNonAD().String()] = true
			}
			settings.BotNamePromptDismissedCacheMu.Unlock()
		}
	}

	_ = b.groupManager.LoadFromDB(sessionCtx, cli)
	calls.RegisterWACaller(cli)

	// Explicit session logout routine
	if b.cfg.Logout {
		logger.Info("initiating session logout", "session", b.cfg.Session)

		if cli.Store.ID == nil {
			logger.Info("session was never paired; skipping server-side revocation")
		} else {
			connected := make(chan struct{}, 1)
			cli.AddEventHandler(func(evt any) {
				if _, ok := evt.(*events.Connected); ok {
					select {
					case connected <- struct{}{}:
					default:
					}
				}
			})

			if err := cli.Connect(); err != nil {
				logger.Warn("Socket connection failed prior to logout; purging local device state only", "err", err)
			} else {
				logoutCtx, logoutCancel := context.WithTimeout(sessionCtx, 10*time.Second)
				select {
				case <-connected:
					logger.Info("connected to WhatsApp routing servers; dispatching logout frame")
				case <-logoutCtx.Done():
					logger.Warn("connection timeout during logout sequence; forcing server revocation")
				}
				logoutCancel()

				if err := cli.Logout(sessionCtx); err != nil {
					logger.Warn("server logout command returned error", "err", err)
				}
				cli.Disconnect()
			}
		}

		b.client.ClearSessionDB(sessionCtx, b.cfg.Session)
		logger.Info("session credentials and records purged successfully", "session", b.cfg.Session)
		return nil
	}

	cli.AddEventHandler(func(evt any) {
		b.WAEventHandler(evt)
	})

	if cli.Store.ID == nil {
		if b.cfg.Pair {
			if err := b.runPairCode(sessionCtx); err != nil {
				return err
			}
		} else {
			go func() {
				if err := b.runQR(sessionCtx); err != nil {
					logger.Error("runQR execution error", "err", err)
				}
			}()
		}
	} else {
		if err := cli.Connect(); err != nil {
			if b.loggedOut.Load() || strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "logged out") {
				logger.Warn("connect rejected due to expired authentication; clearing local device record", "err", err)
				b.client.ClearSessionDB(sessionCtx, "")
				return whatsrook.ErrLoggedOut
			}
			return err
		}
	}

	if b.loggedOut.Load() {
		logger.Warn("session revoked; clearing local device record")
		b.client.ClearSessionDB(sessionCtx, "")
		return whatsrook.ErrLoggedOut
	}

	if s, ok := cli.Store.Identities.(*sqlstore.SQLStore); ok && s != nil {
		group.StartAutoMuteScheduler(sessionCtx, cli)
		settings.StartAutoBioScheduler(sessionCtx, cli)
	}

	go b.startPresenceHeartbeat(sessionCtx, cli)

	for {
		select {
		case <-sessionCtx.Done():
			if b.loggedOut.Load() {
				logger.Warn("session terminated during runtime; purging device record")
				b.client.ClearSessionDB(ctx, "")
				return whatsrook.ErrLoggedOut
			}
			return nil
		case ctrl := <-b.hub.Control:
			ack := b.Controller(sessionCtx, ctrl)
			b.hub.Broadcast(ack)
		}
		if b.loggedOut.Load() {
			logger.Warn("session terminated during runtime; purging device record")
			b.client.ClearSessionDB(ctx, "")
			return whatsrook.ErrLoggedOut
		}
	}
}

func (b *Bot) GetStatsPayload(ctx context.Context) StatsPayload {
	var connected bool
	var loggedIn bool
	var jidStr *string
	var pushName *string
	var botName *string
	defaultPrefix := "."
	prefix := &defaultPrefix
	var mode *string
	var dbContactsCount uint32
	var dbDriver string = "postgres"
	if b.client != nil && b.client.Config.Database != "" {
		dbDriver = b.client.Config.Database
	}
	var anticallEnabled bool
	var likestatusEnabled bool
	var sudoersCount uint32

	cli := b.client.WAClient()
	if cli != nil {
		connected = cli.IsConnected()
		loggedIn = cli.IsLoggedIn()

		if cli.Store != nil && cli.Store.ID != nil {
			str := cli.Store.ID.String()
			jidStr = &str
			if cli.Store.PushName != "" {
				pn := cli.Store.PushName
				pushName = &pn
			}
		}

		if s, ok := cli.Store.Identities.(*sqlstore.SQLStore); ok {
			if contacts, err := s.GetAllContacts(ctx); err == nil {
				dbContactsCount = uint32(len(contacts))
			}

			if bn, err := store.GetSetting(ctx, s, settings.BotNameSettingKey); err == nil && bn != "" {
				botName = &bn
			}
			if p, err := store.GetSetting(ctx, s, settings.PrefixSettingKey); err == nil && p != "" {
				prefix = &p
			}
			if m, err := store.GetSetting(ctx, s, "mode"); err == nil && m != "" {
				mode = &m
			}
			if ac, err := store.GetSetting(ctx, s, "anticall_status"); err == nil && ac == "on" {
				anticallEnabled = true
			}
			if ls, err := store.GetSetting(ctx, s, "likestatus_status"); err == nil && ls == "on" {
				likestatusEnabled = true
			}
			if sudoRaw, err := store.GetSetting(ctx, s, "sudoers"); err == nil && sudoRaw != "" {
				parts := strings.Fields(strings.ReplaceAll(sudoRaw, ",", " "))
				sudoersCount = uint32(len(parts))
			}
		}
	}

	uptimeSec := int64(time.Since(b.startupTime).Seconds())
	uptimeFmt := util.FormatDuration(time.Duration(uptimeSec) * time.Second)

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	memUsed := ms.Alloc
	memUsedFmt := util.FormatBytes(memUsed)

	wsClients := uint32(0)
	if b.hub != nil {
		wsClients = uint32(b.hub.ConnectedClientsCount())
	}

	activePlugins := uint32(dispatch.Count())

	return StatsPayload{
		Connected:           connected,
		LoggedIn:            loggedIn,
		JID:                 jidStr,
		PushName:            pushName,
		BotName:             botName,
		Prefix:              prefix,
		Mode:                mode,
		UptimeSeconds:       uptimeSec,
		UptimeFormatted:     uptimeFmt,
		MemoryUsedBytes:     memUsed,
		MemoryUsedFormatted: memUsedFmt,
		MemorySysBytes:      ms.Sys,
		ActivePluginsCount:  activePlugins,
		ConnectedWSClients:  wsClients,
		PlatformOS:          runtime.GOOS,
		GoVersion:           runtime.Version(),
		AppVersion:          updater.GetAppVersion(),
		SessionPhone:        b.cfg.Session,
		NetworkPaused:       false,
		DBContactsCount:     dbContactsCount,
		DBDriver:            dbDriver,
		AnticallEnabled:     anticallEnabled,
		LikestatusEnabled:   likestatusEnabled,
		SudoersCount:        sudoersCount,
	}
}

func (b *Bot) runPairCode(ctx context.Context) error {
	code, err := b.client.PairPhone(ctx, b.cfg.Session)
	if err != nil {
		return err
	}
	logger.Debug("pair code issued", "code", code)
	logger.Info(fmt.Sprintf("PAIR CODE: %s", code))
	b.hub.Broadcast(EventMessage{
		Kind:    EventPairCode,
		Payload: PairCodePayload{Code: code},
	})
	return nil
}

func (b *Bot) runQR(ctx context.Context) error {
	qrChan, err := b.client.QRChannel(ctx)
	if err != nil {
		return err
	}

	qrServer, err := qr.StartServer()
	if err != nil {
		logger.Warn("failed to start temporary qr server", "err", err)
	} else {
		defer func() {
			_ = qrServer.Close()
			logger.Debug("temporary qr server released", "port", qrServer.Port())
		}()
		logger.Info("temporary QR server started", "url", qrServer.URL())
		if b.cfg.QRCode {
			fmt.Printf("\n==> Scan QR Code interface via browser: %s\n\n", qrServer.URL())
		}
	}

	cli := b.client.WAClient()
	if cli != nil && !cli.IsConnected() {
		if err := cli.Connect(); err != nil {
			logger.Warn("failed to connect socket for QR streaming", "err", err)
		}
	}

	for evt := range qrChan {
		switch evt.Event {
		case "code":
			if qrServer != nil {
				qrServer.UpdateCode(evt.Code)
			}
			if termQR := qr.RenderTerminal(evt.Code); termQR != "" {
				fmt.Printf("\n%s\n", termQR)
			}
			b.hub.Broadcast(EventMessage{
				Kind:    EventPairQR,
				Payload: PairQRPayload{Code: evt.Code},
			})
		case "success":
			if qrServer != nil {
				qrServer.SetPaired()
				time.Sleep(1 * time.Second)
			}
			logger.Info("QR code pairing successful, shutting down temporary QR server")
			return nil
		default:
			logger.Debug("qr event dispatched", "event", evt.Event)
		}
	}

	return nil
}

func (b *Bot) WAEventHandler(evt any) {
	defer func() {
		if r := recover(); r != nil {
			crashPath := util.RecordCrash(r, fmt.Sprintf("WAEventHandler: %T", evt))
			logger.Error("Panic recovered in WhatsApp event handler", "panic", r, "crash_log", crashPath)
		}
	}()

	var cli *whatsmeow.Client
	if b.client != nil {
		cli = b.client.WAClient()
	}

	broadcast := func(msg EventMessage) {
		if b.hub != nil {
			b.hub.Broadcast(msg)
		}
	}

	switch v := evt.(type) {
	case *events.QR:
		_ = v // QR frames handled directly via runQR channel loop

	case *events.PairSuccess:
		logger.Info("pairing completed successfully", "event", v)
		broadcast(simpleEvent(EventPairSuccess))
		// After QR pairing, WhatsApp drops the pairing socket via stream:error 516.
		// PairSuccess fires while the socket is still alive, so we must wait for the
		// disconnect before calling Connect() — whatsmeow does not emit events.Disconnected
		// for expected pair-drops, so we poll IsConnected() with a short deadline instead.
		go func() {
			cli := b.client.WAClient()
			if cli == nil {
				return
			}
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if !cli.IsConnected() {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			if err := cli.Connect(); err != nil {
				logger.Error("failed to reconnect after QR pairing", "err", err)
			}
		}()

	case *events.PairError:
		logger.Warn("pairing procedure failed", "err", v.Error, "event", v)
		broadcast(EventMessage{
			Kind:    EventPairError,
			Payload: PairErrorPayload{Reason: v.Error.Error()},
		})

	case *events.LoggedOut:
		logger.Warn("device logged out by remote session", "reason", v.Reason, "event", v)
		b.loggedOut.Store(true)
		broadcast(simpleEvent(EventLoggedOut))
		b.mu.Lock()
		onLoggedOut := b.onLoggedOut
		b.mu.Unlock()
		if onLoggedOut != nil {
			onLoggedOut()
		}

	case *events.Disconnected:
		logger.Info("Socket connection disconnected", "event", v)
		broadcast(simpleEvent(EventDisconnected))

	case *events.Connected:
		logger.Info("Socket connection established", "session", b.cfg.Session, "event", v)
		broadcast(simpleEvent(EventConnected))
		if cli != nil {
			if len(cli.Store.PushName) == 0 {
				cli.Store.PushName = "WhatsRook"
			}
			cli.SetForceActiveDeliveryReceipts(true)
			if err := cli.SetPassive(context.Background(), false); err != nil {
				logger.Warn("Failed to set browser active", "err", err)
			}
			if err := cli.SendPresence(context.Background(), types.PresenceAvailable); err != nil {
				logger.Warn("Failed to send presence available", "err", err)
			} else {
				logger.Info("Client presence set to online and browser active")
			}

			go func() {
				if err := b.groupManager.SyncAll(context.Background(), cli); err != nil {
					logger.Warn("groupManager.SyncAll returned error", "err", err)
				}
			}()
		}

	case *events.Message:
		logger.Debug("incoming message received", "event", v)
		go func(v *events.Message) {
			msgStart := time.Now()
			msgID := v.Info.ID
			defer func() {
				logger.Debug("[PERF] events.Message handler finished", "msgID", msgID, "elapsed", time.Since(msgStart))
			}()

			if v.Info.Chat.Server == "broadcast" || v.Info.Chat.String() == "status@broadcast" {
				settings.HandleStatusBroadcast(context.Background(), cli, v, b.startupTime)
			}

			if !v.Info.Timestamp.IsZero() && v.Info.Timestamp.Before(b.startupTime) {
				logger.Debug("events.Message: message sent before bot startup, ignoring commands",
					"msgID", v.Info.ID,
					"chat", v.Info.Chat.String(),
					"sender", v.Info.Sender.String(),
					"timestamp", v.Info.Timestamp,
					"startupTime", b.startupTime,
				)
				if dispatch.Dispatch(context.Background(), cli, v) {
					return
				}
				payload := buildIncomingMessagePayload(v)
				b.hub.Broadcast(EventMessage{
					Kind:    EventIncomingMessage,
					Payload: payload,
				})
				return
			}

			t0 := time.Now()
			if calls.HandlePendingAudioReply(context.Background(), cli, v) {
				logger.Debug("[PERF] HandlePendingAudioReply intercepted", "msgID", msgID, "elapsed", time.Since(t0))
				return
			}
			tAudio := time.Since(t0)

			t0 = time.Now()
			if info.HandlePendingMenuMediaReply(context.Background(), cli, v) {
				logger.Debug("[PERF] HandlePendingMenuMediaReply intercepted", "msgID", msgID, "elapsed", time.Since(t0))
				return
			}
			tMenu := time.Since(t0)

			t0 = time.Now()
			if settings.HandlePendingBotCustomizationReply(context.Background(), cli, v) {
				logger.Debug("[PERF] HandlePendingBotCustomizationReply intercepted", "msgID", msgID, "elapsed", time.Since(t0))
				return
			}
			tCustom := time.Since(t0)

			t0 = time.Now()
			if group.HandlePendingCaptchaReply(context.Background(), cli, v) {
				logger.Debug("[PERF] HandlePendingCaptchaReply intercepted", "msgID", msgID, "elapsed", time.Since(t0))
				return
			}
			tCaptcha := time.Since(t0)

			logger.Debug("[PERF] Pre-dispatch reply checks completed",
				"msgID", msgID,
				"audio", tAudio,
				"menu", tMenu,
				"customization", tCustom,
				"captcha", tCaptcha,
			)

			t0 = time.Now()
			if dispatch.Dispatch(context.Background(), cli, v) {
				logger.Debug("[PERF] dispatch.Dispatch completed (handled)", "msgID", msgID, "elapsed", time.Since(t0))
				return
			}
			logger.Debug("[PERF] dispatch.Dispatch completed (unhandled)", "msgID", msgID, "elapsed", time.Since(t0))

			payload := buildIncomingMessagePayload(v)
			b.hub.Broadcast(EventMessage{
				Kind:    EventIncomingMessage,
				Payload: payload,
			})
		}(v)

	case *events.Presence:
		logger.Debug("presence update received", "event", v)
		group.TrackPresence(v.From, !v.Unavailable)

	case *events.ChatPresence:
		logger.Debug("chat presence update received", "event", v)
		group.TrackPresence(v.Sender, true)

	case *events.Receipt:
		if !v.Sender.IsEmpty() {
			group.TrackPresence(v.Sender, true)
		}

	case *events.CallOffer:
		logger.Debug("incoming call offer received", "event", v)
		calls.HandleAntiCallEvent(context.Background(), cli, v, b.startupTime)
		b.hub.Broadcast(EventMessage{
			Kind: EventIncomingCall,
			Payload: IncomingCallPayload{
				CallID:    v.CallID,
				From:      v.CallCreator.String(),
				Timestamp: v.Timestamp,
			},
		})

	case *events.GroupInfo:
		logger.Debug("group metadata update received", "event", v)
		b.groupManager.UpdateFromEvent(context.Background(), cli, v)
		group.HandleGreetings(context.Background(), cli, v, b.startupTime)
		group.HandleEventsNotification(context.Background(), cli, v, b.startupTime)
		group.HandleCaptcha(context.Background(), cli, v, b.startupTime)

	case *events.JoinedGroup:
		logger.Debug("joined group event received", "event", v)
		b.groupManager.UpdateFromEvent(context.Background(), cli, v)

	case *events.Picture:
		b.groupManager.UpdateFromEvent(context.Background(), cli, v)

	case *events.NewsletterJoin:
		logger.Info("newsletter subscribed", "event", v)
		b.groupManager.UpdateFromEvent(context.Background(), cli, v)

	case *events.NewsletterLeave:
		logger.Info("newsletter unlinked", "event", v)
		b.groupManager.UpdateFromEvent(context.Background(), cli, v)

	case *events.NewsletterMuteChange:
		b.groupManager.UpdateFromEvent(context.Background(), cli, v)

	case *events.NewsletterLiveUpdate:
		b.groupManager.UpdateFromEvent(context.Background(), cli, v)

	// Stream, Session & Connection Diagnostics
	case *events.StreamError:
		logger.Error("stream error received", "event", v)
	case *events.KeepAliveTimeout:
		logger.Warn("keepalive ping timed out", "event", v)
	case *events.KeepAliveRestored:
		logger.Info("keepalive connection restored", "event", v)
	case *events.ManualLoginReconnect:
		logger.Info("manual login reconnect triggered", "event", v)
	case *events.QRScannedWithoutMultidevice:
		logger.Warn("qr scanned on legacy non-multidevice client", "event", v)

	// Cryptography & Decryption Failures
	case *events.UndecryptableMessage:
		logger.Warn("undecryptable message received", "event", v)
	case *events.UndecryptedMessage:
		logger.Warn("undecrypted message received", "event", v)
	case *events.MediaRetry:
		logger.Debug("media download retry signal", "event", v)

	// History & App State Synchronizations
	case *events.HistorySync:
		logger.Debug("history synchronization chunk received", "event", v)
	case *events.OfflineSyncPreview:
		logger.Debug("Offline message sync preview", "event", v)
	case *events.OfflineSyncCompleted:
		logger.Debug("Offline message sync completed", "event", v)
	case *events.AppState:
		logger.Debug("app state sync mutation received", "event", v)
	case *events.AppStateSyncComplete:
		logger.Debug("app state sync complete", "event", v)

	// User, Contacts & Privacy Metadata
	case *events.PushName:
		logger.Debug("push name update received", "event", v)
		if cli != nil && cli.IsConnected() && cli.IsLoggedIn() {
			_ = cli.SendPresence(context.Background(), types.PresenceAvailable)
		}
	case *events.UserAbout:
		logger.Debug("user about/status text updated", "event", v)
	case *events.Contact:
		logger.Debug("contact record updated", "event", v)
	case *events.IdentityChange:
		logger.Warn("e2ee identity key changed", "event", v)
	case *events.PrivacySettings:
		logger.Info("account privacy settings updated", "event", v)
	case *events.DisappearingMode:
		logger.Info("disappearing mode updated",
			"chat", v.Chat.String(),
			"timer", v.Timer.String(),
			"is_ephemeral", v.IsEphemeral,
			"trigger", v.Trigger,
			"initiator", v.Initiator,
		)
		b.groupManager.UpdateFromEvent(context.Background(), cli, v)
	case *events.Blocklist:
		logger.Info("blocklist synchronized", "event", v)
	case *events.NotifyAccountReachoutTimelock:
		logger.Warn("account reachout timelock notification", "event", v)

	// Call Signaling Transitions
	case *events.CallOfferNotice:
		logger.Info("call offer notice received", "event", v)
	case *events.CallAccept:
		logger.Info("call accepted", "event", v)
	case *events.CallPreAccept:
		logger.Debug("call pre-accept signal", "event", v)
	case *events.CallRelayLatency:
		logger.Debug("call relay latency update", "event", v)
	case *events.CallTransport:
		logger.Debug("call transport parameters negotiated", "event", v)
	case *events.CallTerminate:
		logger.Info("call terminated", "event", v)
	case *events.CallReject:
		logger.Info("call rejected", "event", v)
	case *events.UnknownCallEvent:
		logger.Debug("unknown call event frame", "event", v)

	default:
		logger.Debug("unhandled event received", "type", fmt.Sprintf("%T", evt), "event", evt)
	}
}

func buildIncomingMessagePayload(v *events.Message) IncomingMessagePayload {
	text := whatsrook.ExtractMessageText(v)
	mediaType := whatsrook.GetMediaType(v.Message)

	var quotedID string
	var quotedText string

	if ext := v.Message.GetExtendedTextMessage(); ext != nil && ext.GetContextInfo() != nil {
		ci := ext.GetContextInfo()
		quotedID = ci.GetStanzaID()
		if ci.QuotedMessage != nil {
			quotedText = whatsrook.ExtractTextFromProto(ci.QuotedMessage)
		}
	}

	return IncomingMessagePayload{
		From:       v.Info.Chat.String(),
		Chat:       v.Info.Chat.String(),
		Sender:     v.Info.Sender.String(),
		Text:       text,
		MessageID:  v.Info.ID,
		PushName:   v.Info.PushName,
		Timestamp:  v.Info.Timestamp,
		IsGroup:    v.Info.IsGroup,
		IsFromMe:   v.Info.IsFromMe,
		MediaType:  mediaType,
		QuotedID:   quotedID,
		QuotedText: quotedText,
	}
}

// startPresenceHeartbeat periodically refreshes presence available and active browser status.
func (b *Bot) startPresenceHeartbeat(ctx context.Context, cli *whatsmeow.Client) {
	ticker := time.NewTicker(4 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if cli != nil && cli.IsConnected() && cli.IsLoggedIn() {
				if len(cli.Store.PushName) == 0 {
					cli.Store.PushName = "WhatsRook"
				}
				cli.SetForceActiveDeliveryReceipts(true)
				_ = cli.SetPassive(ctx, false)
				if err := cli.SendPresence(ctx, types.PresenceAvailable); err != nil {
					logger.Debug("presence heartbeat failed", "err", err)
				} else {
					logger.Debug("presence heartbeat refreshed (online / browser active)")
				}
			}
		}
	}
}
