package external

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"whatsrook/util/logger"

	"whatsrook"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types/events"
)

var validPluginNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

type liveSession struct {
	cancel     context.CancelFunc
	pluginName string
}

// Dispatcher manages the lifecycle, storage, permissions, and execution of external plugins.
type Dispatcher struct {
	pluginDir   string
	registryURL string
	timeout     time.Duration
	liveTimeout time.Duration

	sessionsMu sync.Mutex
	sessions   map[string]*liveSession // key = "chatJID:pluginName"
}

// DefaultDispatcher is the shared default external plugin dispatcher instance.
var DefaultDispatcher = NewDispatcher()

// Option configures a Dispatcher instance.
type Option func(*Dispatcher)

// NewDispatcher initializes a new external plugin Dispatcher.
func NewDispatcher(opts ...Option) *Dispatcher {
	d := &Dispatcher{
		registryURL: DefaultReleaseRegistry,
		timeout:     DefaultPluginTimeout,
		liveTimeout: DefaultLivePluginTimeout,
		sessions:    make(map[string]*liveSession),
	}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// WithPluginDir sets a custom directory for plugin binaries.
func WithPluginDir(dir string) Option {
	return func(d *Dispatcher) {
		d.pluginDir = dir
	}
}

// PluginDir returns the resolved directory where external plugin binaries are stored.
func (d *Dispatcher) PluginDir() (string, error) {
	if d.pluginDir != "" {
		return d.pluginDir, nil
	}
	if env := os.Getenv(DefaultPluginDirEnv); env != "" {
		return filepath.Clean(env), nil
	}
	baseDir := whatsrook.DefaultDataDir()
	return filepath.Join(baseDir, "plugins"), nil
}

// PluginPath returns the absolute filesystem path for a given plugin name (native executable).
func (d *Dispatcher) PluginPath(name string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if !validPluginNamePattern.MatchString(name) {
		return "", fmt.Errorf("invalid plugin name %q: must be alphanumeric (1-64 chars)", name)
	}
	dir, err := d.PluginDir()
	if err != nil {
		return "", err
	}

	if runtime.GOOS == "windows" {
		exePath := filepath.Join(dir, name+".exe")
		if info, err := os.Stat(exePath); err == nil && !info.IsDir() {
			return exePath, nil
		}
		rawPath := filepath.Join(dir, name)
		if info, err := os.Stat(rawPath); err == nil && !info.IsDir() {
			return rawPath, nil
		}
		return exePath, nil
	}

	return filepath.Join(dir, name), nil
}

// IsInstalled returns true if an executable native binary exists for the given name.
func (d *Dispatcher) IsInstalled(name string) bool {
	path, err := d.PluginPath(name)
	if err != nil {
		return false
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return info.Mode().IsRegular()
	}
	return info.Mode().Perm()&0o111 != 0
}

// IsOfficial returns true if name is one of the 13 official WhatsRook external plugins.
func (d *Dispatcher) IsOfficial(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	return slices.Contains(OfficialPlugins, name)
}

// IsPublic returns true if the plugin is marked public (all official plugins or manifest is_public: true).
func (d *Dispatcher) IsPublic(name string) bool {
	if d.IsOfficial(name) {
		return true
	}
	path, err := d.PluginPath(name)
	if err != nil {
		return false
	}
	manifest := d.readManifest(path)
	return manifest.IsPublic
}

// sessionKey creates a composite lookup key for active live sessions.
func (d *Dispatcher) sessionKey(chatJID, pluginName string) string {
	return chatJID + ":" + pluginName
}

func (d *Dispatcher) registerSession(key string, cancel context.CancelFunc, name string) {
	d.sessionsMu.Lock()
	defer d.sessionsMu.Unlock()
	d.sessions[key] = &liveSession{cancel: cancel, pluginName: name}
}

func (d *Dispatcher) unregisterSession(key string) {
	d.sessionsMu.Lock()
	defer d.sessionsMu.Unlock()
	delete(d.sessions, key)
}

// CancelSession terminates any running streaming session for a chat and plugin.
func (d *Dispatcher) CancelSession(chatJID, pluginName string) bool {
	key := d.sessionKey(chatJID, pluginName)
	d.sessionsMu.Lock()
	defer d.sessionsMu.Unlock()
	sess, ok := d.sessions[key]
	if ok && sess != nil {
		if sess.cancel != nil {
			sess.cancel()
		}
		delete(d.sessions, key)
	}
	return ok
}

// Dispatch executes an external plugin command asynchronously, routing rich context and handling responses.
func (d *Dispatcher) Dispatch(ctx context.Context, client *whatsmeow.Client, evt *events.Message, name string, args []string, rawArgs string) bool {
	path, err := d.PluginPath(name)
	if err != nil {
		return false
	}
	if !d.IsInstalled(name) {
		return false
	}

	chatKey := evt.Info.Chat.String()

	// Check for stop / cancel requests
	isCancelRequest := len(args) > 0 && isStopKeyword(args[0])
	if isCancelRequest {
		if d.CancelSession(chatKey, name) {
			go func() {
				_ = (&whatsrook.PluginContext{
					Ctx: context.Background(), Client: client, Evt: evt,
					Chat: evt.Info.Chat, Sender: evt.Info.Sender,
				}).Replyf("🛑 Live %s tracking stopped.", name)
			}()
		} else {
			go func() {
				_ = (&whatsrook.PluginContext{
					Ctx: context.Background(), Client: client, Evt: evt,
					Chat: evt.Info.Chat, Sender: evt.Info.Sender,
				}).Replyf("No active %s session running in this chat.", name)
			}()
		}
		return true
	}

	// Cancel existing session for this chat & command if already running
	d.CancelSession(chatKey, name)

	// Launch execution in a non-blocking background goroutine
	go func() {
		plugCtx := &whatsrook.PluginContext{
			Ctx:     ctx,
			Client:  client,
			Evt:     evt,
			Chat:    evt.Info.Chat,
			Sender:  evt.Info.Sender,
			Command: name,
			Args:    args,
			RawArgs: rawArgs,
		}

		prefix := plugCtx.GetPrefix()
		botName := plugCtx.GetBotName()
		isSudo := whatsrook.IsSudoRaw(ctx, client, evt.Info.Sender)
		isOwner := plugCtx.IsOwner()

		// Extract quoted message context if present
		var quotedPayload *QuotedMessagePayload
		if qm := plugCtx.GetQuotedMessage(); qm != nil {
			quotedText := whatsrook.ExtractTextFromProto(qm)
			senderJID, _ := plugCtx.GetQuotedSender()
			var stanzaID string
			if ci := plugCtx.GetContextInfo(); ci != nil {
				stanzaID = ci.GetStanzaID()
			}
			quotedPayload = &QuotedMessagePayload{
				ID:     stanzaID,
				Sender: senderJID.String(),
				Text:   quotedText,
			}
		}

		// Extract mentions
		var mentionStrs []string
		for _, jid := range plugCtx.GetMentionedJIDs() {
			mentionStrs = append(mentionStrs, jid.String())
		}

		logger.Debug("external dispatcher: executing plugin", "plugin", name, "chat", chatKey)

		// Extract media if present in the triggering or quoted message
		var mediaPayload *MediaPayload
		var tempMediaFile string
		mediaData, mime, err := plugCtx.GetMedia()
		if err != nil {
			logger.Debug("external dispatcher: GetMedia result", "plugin", name, "err", err)
		} else if len(mediaData) > 0 {
			logger.Debug("external dispatcher: GetMedia found media", "plugin", name, "mime", mime, "bytes", len(mediaData))
			ext := ".bin"
			switch {
			case strings.Contains(mime, "image/jpeg") || strings.Contains(mime, "image/jpg"):
				ext = ".jpg"
			case strings.Contains(mime, "image/png"):
				ext = ".png"
			case strings.Contains(mime, "image/webp"):
				ext = ".webp"
			case strings.Contains(mime, "video/mp4"):
				ext = ".mp4"
			case strings.Contains(mime, "audio/ogg"):
				ext = ".ogg"
			case strings.Contains(mime, "audio/mp4") || strings.Contains(mime, "audio/m4a"):
				ext = ".m4a"
			case strings.Contains(mime, "audio/mpeg") || strings.Contains(mime, "audio/mp3"):
				ext = ".mp3"
			}

			if tmp, err := os.CreateTemp("", "whatsrook_media_*"+ext); err == nil {
				if _, err := tmp.Write(mediaData); err == nil {
					tempMediaFile = tmp.Name()
					isQuoted := plugCtx.GetQuotedMessage() != nil
					mediaPayload = &MediaPayload{
						Path:     tempMediaFile,
						MimeType: mime,
						IsQuoted: isQuoted,
					}
					logger.Debug("external dispatcher: saved media to temp file", "path", tempMediaFile)
				}
				_ = tmp.Close()
			}
		}

		if tempMediaFile != "" {
			defer os.Remove(tempMediaFile)
		}

		stickerPack := plugCtx.GetStickerPack()
		stickerAuthor := plugCtx.GetStickerAuthor()

		effectiveArgs := args
		effectiveRawArgs := rawArgs
		if (name == "sticker" || name == "circle" || name == "crop" || name == "take") && strings.TrimSpace(rawArgs) == "" {
			effectiveRawArgs = stickerAuthor + "|" + stickerPack
			effectiveArgs = []string{effectiveRawArgs}
		}

		req := Request{
			Command:         name,
			Args:            effectiveArgs,
			RawArgs:         effectiveRawArgs,
			Chat:            chatKey,
			Sender:          evt.Info.Sender.String(),
			Prefix:          prefix,
			BotName:         botName,
			PushName:        evt.Info.PushName,
			IsGroup:         evt.Info.IsGroup,
			IsSudo:          isSudo,
			IsOwner:         isOwner,
			IsLiveSession:   false,
			IsCancelRequest: false,
			QuotedMessage:   quotedPayload,
			MentionedJIDs:   mentionStrs,
			Media:           mediaPayload,
			StickerPack:     stickerPack,
			StickerAuthor:   stickerAuthor,
		}

		d.runProcess(plugCtx, path, name, req)
	}()

	return true
}

// Install downloads or copies an executable into the managed plugin directory and creates its metadata manifest.
func (d *Dispatcher) Install(ctx context.Context, name string, source string) error {
	name = strings.ToLower(strings.TrimSpace(name))
	if !validPluginNamePattern.MatchString(name) {
		return fmt.Errorf("invalid plugin name %q: must be alphanumeric (1-64 characters)", name)
	}

	dir, err := d.PluginDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create plugin dir: %w", err)
	}

	target := filepath.Join(dir, name)
	if runtime.GOOS == "windows" {
		target = filepath.Join(dir, name+".exe")
	}

	tmpPath := target + ".tmp"
	tmp, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o700)
	if err != nil {
		return fmt.Errorf("create temp plugin file: %w", err)
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()

	var reader io.Reader
	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
		if err != nil {
			return fmt.Errorf("create download request: %w", err)
		}
		req.Header.Set("User-Agent", "WhatsRook-Plugin-Installer/1.0")

		client := &http.Client{Timeout: 60 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("download plugin binary: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("download returned HTTP status %d", resp.StatusCode)
		}
		reader = io.LimitReader(resp.Body, MaxPluginBinarySize+1)
	} else {
		file, err := os.Open(filepath.Clean(source))
		if err != nil {
			return fmt.Errorf("open local source binary: %w", err)
		}
		defer file.Close()
		reader = io.LimitReader(file, MaxPluginBinarySize+1)
	}

	written, err := io.Copy(tmp, reader)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("copy plugin binary: %w", err)
	}
	if written == 0 || written > MaxPluginBinarySize {
		return fmt.Errorf("plugin binary size must be between 1 byte and %d MiB", MaxPluginBinarySize/(1<<20))
	}

	if err := extractArchiveIfNeeded(tmpPath, name); err != nil {
		return fmt.Errorf("extract plugin archive: %w", err)
	}

	if runtime.GOOS == "windows" {
		target = filepath.Join(dir, name+".exe")
	} else {
		target = filepath.Join(dir, name)
	}

	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return fmt.Errorf("chmod plugin: %w", err)
	}

	_ = os.Remove(target)
	if runtime.GOOS == "windows" {
		_ = os.Remove(filepath.Join(dir, name))
	}

	if err := os.Rename(tmpPath, target); err != nil {
		return fmt.Errorf("install plugin binary: %w", err)
	}

	isPub := d.IsOfficial(name)
	manifest := Manifest{Name: name, IsPublic: isPub}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		_ = os.Remove(target)
		return fmt.Errorf("marshal manifest: %w", err)
	}
	manifestPath := filepath.Join(dir, name+".json")
	if err := os.WriteFile(manifestPath, append(data, '\n'), 0o600); err != nil {
		_ = os.Remove(target)
		return fmt.Errorf("write manifest: %w", err)
	}

	return nil
}

// InstallAll downloads and installs all official plugins in parallel using concurrent workers.
func (d *Dispatcher) InstallAll(ctx context.Context) ([]string, []string) {
	type result struct {
		name string
		err  error
	}
	resChan := make(chan result, len(OfficialPlugins))
	var wg sync.WaitGroup

	for _, name := range OfficialPlugins {
		wg.Add(1)
		go func(plugName string) {
			defer wg.Done()
			url, err := d.ResolveDefaultPluginURL(plugName)
			if err != nil {
				resChan <- result{name: plugName, err: err}
				return
			}
			err = d.Install(ctx, plugName, url)
			resChan <- result{name: plugName, err: err}
		}(name)
	}

	wg.Wait()
	close(resChan)

	var installed []string
	var failed []string
	for res := range resChan {
		if res.err != nil {
			failed = append(failed, whatsrook.Sprintf("%s (%v)", res.name, res.err))
		} else {
			installed = append(installed, res.name)
		}
	}
	sort.Strings(installed)
	sort.Strings(failed)

	return installed, failed
}

// Uninstall removes an installed plugin binary and its JSON manifest.
func (d *Dispatcher) Uninstall(name string) error {
	path, err := d.PluginPath(name)
	if err != nil {
		return err
	}
	if !d.IsInstalled(name) {
		return fmt.Errorf("plugin %q is not installed", name)
	}
	dir, _ := d.PluginDir()
	_ = os.Remove(path)
	_ = os.Remove(filepath.Join(dir, name))
	_ = os.Remove(filepath.Join(dir, name+".exe"))
	_ = os.Remove(filepath.Join(dir, name+".json"))
	return nil
}

// UninstallAll removes all currently installed external plugins.
func (d *Dispatcher) UninstallAll() ([]string, error) {
	plugins, err := d.List()
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, plug := range plugins {
		if err := d.Uninstall(plug.Name); err == nil {
			removed = append(removed, plug.Name)
		}
	}
	return removed, nil
}

// List returns all currently installed external plugins sorted alphabetically.
func (d *Dispatcher) List() ([]PluginInfo, error) {
	dir, err := d.PluginDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	var list []PluginInfo
	seen := make(map[string]bool)

	for _, entry := range entries {
		if entry.IsDir() || strings.HasSuffix(entry.Name(), ".json") || strings.HasSuffix(entry.Name(), ".tmp") || strings.HasSuffix(entry.Name(), ".extracted") {
			continue
		}
		cleanName := strings.TrimSuffix(strings.ToLower(entry.Name()), ".exe")
		if seen[cleanName] {
			continue
		}

		fullPath := filepath.Join(dir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if runtime.GOOS == "windows" {
			if !info.Mode().IsRegular() {
				continue
			}
		} else if info.Mode().Perm()&0o111 == 0 {
			continue
		}
		seen[cleanName] = true

		manifest := d.readManifest(fullPath)
		if manifest.Name == "" {
			manifest = d.readManifest(filepath.Join(dir, cleanName))
		}
		list = append(list, PluginInfo{
			Name:        cleanName,
			Path:        fullPath,
			Description: manifest.Description,
			IsPublic:    d.IsOfficial(cleanName) || manifest.IsPublic,
			Size:        info.Size(),
			ModTime:     info.ModTime(),
		})
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].Name < list[j].Name
	})
	return list, nil
}

func (d *Dispatcher) readManifest(binaryPath string) Manifest {
	var m Manifest
	data, err := os.ReadFile(binaryPath + ".json")
	if err != nil {
		base := strings.TrimSuffix(binaryPath, ".exe")
		data, err = os.ReadFile(base + ".json")
	}
	if err == nil {
		_ = json.Unmarshal(data, &m)
	}
	return m
}

func extractArchiveIfNeeded(archivePath, name string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	header := make([]byte, 4)
	n, err := f.Read(header)
	_ = f.Close()
	if err != nil || n < 2 {
		return nil
	}

	if n >= 4 && header[0] == 0x50 && header[1] == 0x4B && header[2] == 0x03 && header[3] == 0x04 {
		return extractBinaryFromZip(archivePath, name)
	}
	if header[0] == 0x1F && header[1] == 0x8B {
		return extractBinaryFromTarGz(archivePath, name)
	}
	return nil
}

func extractBinaryFromZip(archivePath, name string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer zr.Close()

	nameLower := strings.ToLower(name)
	var matchedFile *zip.File
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		base := strings.ToLower(filepath.Base(f.Name))
		baseNoExt := strings.TrimSuffix(base, ".exe")
		if base == nameLower || baseNoExt == nameLower || strings.Contains(base, nameLower) {
			matchedFile = f
			break
		}
	}
	if matchedFile == nil {
		return nil
	}

	rc, err := matchedFile.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	extractedTmp := archivePath + ".extracted"
	out, err := os.OpenFile(extractedTmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, rc); err != nil {
		_ = out.Close()
		_ = os.Remove(extractedTmp)
		return err
	}
	_ = out.Close()
	_ = os.Remove(archivePath)
	return os.Rename(extractedTmp, archivePath)
}

func extractBinaryFromTarGz(archivePath, name string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	gzr, err := gzip.NewReader(f)
	if err != nil {
		return nil
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	nameLower := strings.ToLower(name)
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		base := strings.ToLower(filepath.Base(hdr.Name))
		baseNoExt := strings.TrimSuffix(base, ".exe")
		if base == nameLower || baseNoExt == nameLower || strings.Contains(base, nameLower) {
			extractedTmp := archivePath + ".extracted"
			out, err := os.OpenFile(extractedTmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				_ = out.Close()
				_ = os.Remove(extractedTmp)
				return err
			}
			_ = out.Close()
			_ = f.Close()
			_ = os.Remove(archivePath)
			return os.Rename(extractedTmp, archivePath)
		}
	}
	return nil
}

// ResolvePlatformSuffix maps host OS and architecture to standard release binary naming.
func (d *Dispatcher) ResolvePlatformSuffix() (string, error) {
	osName := runtime.GOOS
	archName := runtime.GOARCH

	switch osName {
	case "linux", "android":
		switch archName {
		case "amd64", "x86_64":
			return "linux-amd64", nil
		case "arm64", "aarch64":
			return "linux-arm64", nil
		default:
			return "", fmt.Errorf("unsupported linux/android architecture: %s", archName)
		}
	case "darwin":
		switch archName {
		case "arm64", "aarch64":
			return "darwin-arm64", nil
		case "amd64", "x86_64":
			return "darwin-amd64", nil
		default:
			return "", fmt.Errorf("unsupported darwin architecture: %s", archName)
		}
	case "windows":
		switch archName {
		case "amd64", "x86_64":
			return "windows-amd64.exe", nil
		case "arm64", "aarch64":
			return "windows-arm64.exe", nil
		default:
			return "", fmt.Errorf("unsupported windows architecture: %s", archName)
		}
	default:
		return "", fmt.Errorf("unsupported operating system: %s", osName)
	}
}

// ResolveDefaultPluginURL resolves the download URL for an official plugin on the host platform.
func (d *Dispatcher) ResolveDefaultPluginURL(name string) (string, error) {
	suffix, err := d.ResolvePlatformSuffix()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/%s-%s", d.registryURL, name, suffix), nil
}

// NormalizePluginSource appends the platform suffix to clean URLs if missing.
func (d *Dispatcher) NormalizePluginSource(source string) (string, error) {
	suffix, err := d.ResolvePlatformSuffix()
	if err != nil {
		return source, err
	}

	source = strings.ReplaceAll(source, "{platform}", suffix)
	source = strings.ReplaceAll(source, "{target}", suffix)
	source = strings.ReplaceAll(source, "{suffix}", suffix)

	if !strings.HasPrefix(source, "http://") && !strings.HasPrefix(source, "https://") {
		return source, nil
	}

	knownSuffixes := []string{
		"linux-amd64", "linux-arm64", "linux-musl-amd64", "linux-musl-arm64",
		"darwin-amd64", "darwin-arm64", "windows-amd64.exe", "windows-arm64.exe",
		".exe", ".tar.gz", ".zip",
	}
	lowerSource := strings.ToLower(source)
	for _, ks := range knownSuffixes {
		if strings.HasSuffix(lowerSource, ks) {
			return source, nil
		}
	}

	source = strings.TrimSuffix(source, "/")
	return source + "-" + suffix, nil
}

func isStopKeyword(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "stop" || s == "cancel" || s == "end" || s == "off"
}
