package main

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

func runProto(args []string) error {
	rootDir, err := findRepoRoot()
	if err != nil {
		return fmt.Errorf("error finding project root: %w", err)
	}

	protoDir := filepath.Join(rootDir, "lib", "proto")
	if _, err := os.Stat(protoDir); os.IsNotExist(err) {
		return fmt.Errorf("protobuf directory not found at: %s", protoDir)
	}

	// 1. Check if upstream schema synchronization was requested
	isSync := false
	var filteredArgs []string
	for _, a := range args {
		la := strings.ToLower(a)
		if la == "--sync" || la == "-sync" || la == "sync" || la == "--update" || la == "update" {
			isSync = true
		} else {
			filteredArgs = append(filteredArgs, a)
		}
	}

	if isSync {
		if err := syncProtosFromWaProto(rootDir, protoDir); err != nil {
			return fmt.Errorf("upstream protobuf synchronization failed: %w", err)
		}
	}

	// 2. Setup PATH with Go bin directory (GOPATH/bin, GOBIN)
	setupGoBinPath()

	// 3. Ensure protoc-gen-go plugin is installed and accessible
	if err := ensureProtocGenGo(); err != nil {
		return err
	}

	// 4. Check for protoc compiler
	protocPath, err := exec.LookPath("protoc")
	if err != nil {
		printProtocInstallInstructions()
		return fmt.Errorf("protoc compiler not found in PATH")
	}

	// 5. Sanitize proto definitions to prevent enum collision issues
	if err := sanitizeProtoDefinitions(protoDir); err != nil {
		return fmt.Errorf("failed to sanitize proto definitions: %w", err)
	}

	// 6. Normalize option go_package in proto files (ensure go.mau.fi/whatsmeow/proto/...)
	if err := normalizeProtoPackageOptions(protoDir); err != nil {
		return fmt.Errorf("failed to normalize proto go_package options: %w", err)
	}

	// 7. Find all .proto files (with optional filter)
	var protoFiles []string
	targetFilter := ""
	if len(filteredArgs) > 0 {
		targetFilter = strings.ToLower(filteredArgs[0])
	}

	err = filepath.WalkDir(protoDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".proto") {
			relPath, relErr := filepath.Rel(protoDir, path)
			if relErr == nil {
				if targetFilter == "" || strings.Contains(strings.ToLower(relPath), targetFilter) {
					protoFiles = append(protoFiles, relPath)
				}
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to scan proto files: %w", err)
	}

	if len(protoFiles) == 0 {
		fmt.Println("No .proto files found matching criteria.")
		return nil
	}

	fmt.Printf("Found %d protobuf file(s) to compile using %s...\n", len(protoFiles), protocPath)

	// 6. Execute protoc for each proto file
	successCount := 0
	var failedFiles []string

	for _, rel := range protoFiles {
		protocArgs := []string{
			"--proto_path=" + protoDir,
			"--go_out=" + protoDir,
			"--go_opt=paths=source_relative",
			rel,
		}

		cmd := exec.Command("protoc", protocArgs...)
		cmd.Dir = protoDir
		cmd.Env = os.Environ()

		output, err := cmd.CombinedOutput()
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Error compiling %s: %v\n%s\n", rel, err, string(output))
			failedFiles = append(failedFiles, rel)
		} else {
			fmt.Printf("✓ Compiled %s\n", rel)
			successCount++
		}
	}

	fmt.Printf("\nProtobuf update complete: %d/%d files compiled successfully.\n", successCount, len(protoFiles))
	if len(failedFiles) > 0 {
		return fmt.Errorf("%d of %d protobuf file(s) failed to compile: %s", len(failedFiles), len(protoFiles), strings.Join(failedFiles, ", "))
	}

	return nil
}

func setupGoBinPath() {
	goBinDir := getGoBinDir()
	if goBinDir == "" {
		return
	}

	currPath := os.Getenv("PATH")
	paths := filepath.SplitList(currPath)
	for _, p := range paths {
		if p == goBinDir {
			return
		}
	}

	_ = os.Setenv("PATH", goBinDir+string(os.PathListSeparator)+currPath)
}

func getGoBinDir() string {
	if gobin := os.Getenv("GOBIN"); gobin != "" {
		return gobin
	}

	if out, err := exec.Command("go", "env", "GOBIN").Output(); err == nil {
		if s := strings.TrimSpace(string(out)); s != "" {
			return s
		}
	}

	if gopath := os.Getenv("GOPATH"); gopath != "" {
		return filepath.Join(gopath, "bin")
	}

	if out, err := exec.Command("go", "env", "GOPATH").Output(); err == nil {
		if s := strings.TrimSpace(string(out)); s != "" {
			return filepath.Join(s, "bin")
		}
	}

	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, "go", "bin")
	}

	return ""
}

func ensureProtocGenGo() error {
	if _, err := exec.LookPath("protoc-gen-go"); err == nil {
		return nil
	}

	goBinDir := getGoBinDir()
	if goBinDir != "" {
		binPath := filepath.Join(goBinDir, "protoc-gen-go")
		if runtime.GOOS == "windows" {
			binPath += ".exe"
		}
		if _, err := os.Stat(binPath); err == nil {
			return nil
		}
	}

	fmt.Println("Installing protoc-gen-go plugin...")
	cmd := exec.Command("go", "install", "google.golang.org/protobuf/cmd/protoc-gen-go@latest")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to install protoc-gen-go plugin: %w", err)
	}

	setupGoBinPath()
	if _, err := exec.LookPath("protoc-gen-go"); err != nil {
		return fmt.Errorf("protoc-gen-go was installed but could not be located in PATH")
	}

	return nil
}

func normalizeProtoPackageOptions(protoDir string) error {
	const legacyPrefix = "github.com/polymorfa/hypermeow/proto/"
	const targetPrefix = "go.mau.fi/whatsmeow/proto/"

	return filepath.WalkDir(protoDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".proto") {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		if bytes.Contains(data, []byte(legacyPrefix)) {
			updated := bytes.ReplaceAll(data, []byte(legacyPrefix), []byte(targetPrefix))
			if err := os.WriteFile(path, updated, 0644); err != nil {
				return err
			}
		}

		return nil
	})
}

var canonicalWaE2EEnums = map[string]bool{
	"HistorySyncType":              true,
	"InsightDeliveryState":         true,
	"KeepType":                     true,
	"MediaKeyDomain":               true,
	"PeerDataOperationRequestType": true,
	"PollContentType":              true,
	"PollType":                     true,
	"WebLinkRenderConfig":          true,
}

func sanitizeProtoDefinitions(protoDir string) error {
	referencedTypes := make(map[string]bool)
	fieldTypeRe := regexp.MustCompile(`(?m)^\s*(?:optional|repeated|required)\s+([A-Za-z0-9_.]+)\s+`)
	mapValTypeRe := regexp.MustCompile(`(?m)^\s*map\s*<\s*([A-Za-z0-9_.]+)\s*,\s*([A-Za-z0-9_.]+)\s*>`)

	err := filepath.WalkDir(protoDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".proto") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range fieldTypeRe.FindAllSubmatch(data, -1) {
			t := string(m[1])
			if idx := strings.LastIndex(t, "."); idx != -1 {
				t = t[idx+1:]
			}
			referencedTypes[t] = true
		}
		for _, m := range mapValTypeRe.FindAllSubmatch(data, -1) {
			for _, idxKey := range []int{1, 2} {
				t := string(m[idxKey])
				if idx := strings.LastIndex(t, "."); idx != -1 {
					t = t[idx+1:]
				}
				referencedTypes[t] = true
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed scanning referenced proto types: %w", err)
	}

	waCommonFile := filepath.Join(protoDir, "waCommon", "WACommon.proto")
	if _, err := os.Stat(waCommonFile); err == nil {
		if err := sanitizeWaCommonProto(waCommonFile); err != nil {
			return fmt.Errorf("failed sanitizing waCommon proto: %w", err)
		}
	}

	waHistorySyncFile := filepath.Join(protoDir, "waHistorySync", "WAWebProtobufsHistorySync.proto")
	if _, err := os.Stat(waHistorySyncFile); err == nil {
		if err := sanitizeWaHistorySyncProto(waHistorySyncFile); err != nil {
			return fmt.Errorf("failed sanitizing waHistorySync proto: %w", err)
		}
	}

	waE2EFile := filepath.Join(protoDir, "waE2E", "WAWebProtobufsE2E.proto")
	if _, err := os.Stat(waE2EFile); err == nil {
		if err := sanitizeWaE2EProto(waE2EFile); err != nil {
			return fmt.Errorf("failed sanitizing waE2E proto: %w", err)
		}
	}

	waBotMetadataFile := filepath.Join(protoDir, "waBotMetadata", "WABotMetadata.proto")
	if _, err := os.Stat(waBotMetadataFile); err == nil {
		if err := sanitizeWaBotMetadataProto(waBotMetadataFile); err != nil {
			return fmt.Errorf("failed sanitizing waBotMetadata proto: %w", err)
		}
	}

	return nil
}

func sanitizeWaCommonProto(filePath string) error {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}
	content := string(data)
	if strings.Contains(content, "enum Trigger {") {
		content = strings.Replace(content, "enum Trigger {", "enum TriggerType {", 1)
		content = strings.Replace(content, "optional Trigger trigger = 2;", "optional TriggerType trigger = 2;", 1)
		if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
			return err
		}
		fmt.Printf("✓ Sanitized %s (updated Trigger enum to TriggerType)\n", filepath.Base(filePath))
	}
	return nil
}

func sanitizeWaHistorySyncProto(filePath string) error {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}
	content := string(data)
	if strings.Contains(content, "WACommon.LimitSharing.Trigger limitSharingTrigger") {
		content = strings.ReplaceAll(content, "WACommon.LimitSharing.Trigger limitSharingTrigger", "WACommon.LimitSharing.TriggerType limitSharingTrigger")
		if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
			return err
		}
		fmt.Printf("✓ Sanitized %s (updated LimitSharing.Trigger to LimitSharing.TriggerType)\n", filepath.Base(filePath))
	}
	return nil
}

func sanitizeWaBotMetadataProto(filePath string) error {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}
	content := string(data)
	if strings.Contains(content, "WA_BOT_MSG = 0;") && strings.Contains(content, "WA_BOT_MSG = 1;") {
		content = strings.Replace(content, "WA_BOT_MSG = 0;", "UNSPECIFIED = 0;", 1)
		if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
			return err
		}
		fmt.Printf("✓ Sanitized %s (updated BotSignatureUseCase duplicate enum)\n", filepath.Base(filePath))
	}
	return nil
}

func sanitizeWaE2EProto(filePath string) error {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}

	content := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(content, "\n")
	var cleanedLines []string
	inTopLevelEnum := false
	keepCurrentEnum := false
	prunedCount := 0

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Only match top-level enums (starting at column 0 with no leading indentation)
		if strings.HasPrefix(line, "enum ") && strings.HasSuffix(trimmed, "{") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				enumName := parts[1]
				inTopLevelEnum = true
				// For waE2E, preserve the canonical protocol enums.
				// Any unreferenced or shadowed webpack bundle enums are pruned.
				keepCurrentEnum = canonicalWaE2EEnums[enumName]
				if keepCurrentEnum {
					cleanedLines = append(cleanedLines, line)
				} else {
					prunedCount++
				}
				continue
			}
		}

		if inTopLevelEnum {
			// Check if closing brace of top-level enum (at column 0 without leading whitespace)
			if (trimmed == "}" || trimmed == "};") && !strings.HasPrefix(line, "\t") && !strings.HasPrefix(line, " ") {
				if keepCurrentEnum {
					cleanedLines = append(cleanedLines, line)
				}
				inTopLevelEnum = false
				keepCurrentEnum = false
			} else if keepCurrentEnum {
				cleanedLines = append(cleanedLines, line)
			}
			continue
		}

		cleanedLines = append(cleanedLines, line)
	}

	res := strings.Join(cleanedLines, "\n")
	res = regexp.MustCompile(`\n{3,}`).ReplaceAllString(res, "\n\n")

	modified := prunedCount > 0

	// Scoping fixes: strip incorrect Message. prefix for types that are top-level messages
	scopingFixes := []struct {
		oldStr string
		newStr string
	}{
		{"Message.ChatAnimatedWallpaper", "ChatAnimatedWallpaper"},
		{"Message.FutureProofMessage", "FutureProofMessage"},
		{"Message.SharedDeviceContactHashKeyShare", "SharedDeviceContactHashKeyShare"},
		{"Message.SharedDeviceContactHashKeyRequest", "SharedDeviceContactHashKeyRequest"},
	}
	for _, fix := range scopingFixes {
		if strings.Contains(res, fix.oldStr) {
			res = strings.ReplaceAll(res, fix.oldStr, fix.newStr)
			modified = true
		}
	}

	// Ensure missing top-level message definitions are present in waE2E
	if !strings.Contains(res, "message ChatAnimatedWallpaper {") {
		chatAnimatedWallpaperDef := `message ChatAnimatedWallpaper {
	optional string animatedWallpaperId = 1;
	optional float dimLevel = 2;
}

`
		if idx := strings.Index(res, "message ChatCustomImageWallpaper {"); idx != -1 {
			res = res[:idx] + chatAnimatedWallpaperDef + res[idx:]
		} else {
			res += "\n" + chatAnimatedWallpaperDef
		}
		modified = true
	}

	if !strings.Contains(res, "message SharedDeviceContactHashKey {") {
		sharedDeviceDefs := `message SharedDeviceContactHashKey {
	enum Kind {
		UNKNOWN = 0;
		LID = 1;
		PHONE_NUMBER = 2;
	}

	optional uint32 epoch = 1;
	optional Kind kind = 2;
	optional bytes keyData = 3;
}

message SharedDeviceContactHashKeyRequest {
	optional uint32 knownEpoch = 1;
}

message SharedDeviceContactHashKeyShare {
	repeated SharedDeviceContactHashKey keys = 1;
}

`
		if idx := strings.Index(res, "message SessionStructure {"); idx != -1 {
			res = res[:idx] + sharedDeviceDefs + res[idx:]
		} else {
			res += "\n" + sharedDeviceDefs
		}
		modified = true
	}

	if !modified {
		return nil
	}

	if err := os.WriteFile(filePath, []byte(res), 0644); err != nil {
		return err
	}

	if prunedCount > 0 {
		fmt.Printf("✓ Sanitized %s (pruned %d unreferenced top-level enums)\n", filepath.Base(filePath), prunedCount)
	} else {
		fmt.Printf("✓ Sanitized %s\n", filepath.Base(filePath))
	}
	return nil
}

func printProtocInstallInstructions() {
	fmt.Fprintln(os.Stderr, "❌ Error: 'protoc' (Protocol Buffer Compiler) is not installed or not found in PATH.")
	fmt.Fprintln(os.Stderr, "\nPlease install protoc using your package manager:")
	switch runtime.GOOS {
	case "darwin":
		fmt.Fprintln(os.Stderr, "  brew install protobuf")
	case "linux":
		fmt.Fprintln(os.Stderr, "  Debian/Ubuntu: sudo apt-get install protobuf-compiler")
		fmt.Fprintln(os.Stderr, "  Fedora/RHEL:   sudo dnf install protobuf-compiler")
		fmt.Fprintln(os.Stderr, "  Arch Linux:    sudo pacman -S protobuf")
		fmt.Fprintln(os.Stderr, "  Homebrew:      brew install protobuf")
	case "windows":
		fmt.Fprintln(os.Stderr, "  winget install ProtocolBuffers.Protoc")
		fmt.Fprintln(os.Stderr, "  scoop install protobuf")
		fmt.Fprintln(os.Stderr, "  choco install protoc")
	}
	fmt.Fprintln(os.Stderr, "\nOr download official binary releases from:")
	fmt.Fprintln(os.Stderr, "  https://github.com/protocolbuffers/protobuf/releases")
}

func syncProtosFromWaProto(rootDir, protoDir string) error {
	clientPayloadPath := filepath.Join(rootDir, "lib", "store", "clientpayload.go")

	// 1. Locate or fetch WAProto.proto
	protoSource := ""
	var cleanupTemp func()

	localWaProto := filepath.Join(rootDir, "..", "wa-proto")
	if stat, err := os.Stat(filepath.Join(localWaProto, "WAProto.proto")); err == nil && !stat.IsDir() {
		localPath := filepath.Join(localWaProto, "WAProto.proto")
		if isValidProtoSchema(localPath) {
			protoSource = localPath
			fmt.Printf("Using valid local WAProto schema from %s\n", protoSource)
		}
	}

	if protoSource == "" {
		// Download from GitHub with fallback
		fmt.Println("Downloading latest WAProto.proto from github.com/thruqe/WAProto...")
		downloadedPath, cleanup, err := fetchRemoteWaProto()
		if err != nil {
			return fmt.Errorf("failed fetching remote WAProto.proto: %w", err)
		}
		protoSource = downloadedPath
		cleanupTemp = cleanup
	}
	if cleanupTemp != nil {
		defer cleanupTemp()
	}

	// Validate protoSource has sufficient message definitions before continuing
	if err := validateProtoSchema(protoSource); err != nil {
		return fmt.Errorf("validation failed for %s: %w", protoSource, err)
	}

	// 2. Run wa-proto split command
	fmt.Println("Splitting WAProto into modular lib/proto packages...")
	var splitCmd *exec.Cmd
	if _, err := os.Stat(filepath.Join(localWaProto, "main.go")); err == nil {
		splitCmd = exec.Command("go", "run", ".", "split",
			"-proto", protoSource,
			"-out", protoDir,
			"-clientpayload", clientPayloadPath,
		)
		splitCmd.Dir = localWaProto
	} else {
		splitCmd = exec.Command("go", "run", "github.com/thruqe/WAProto@latest", "split",
			"-proto", protoSource,
			"-out", protoDir,
			"-clientpayload", clientPayloadPath,
		)
		splitCmd.Dir = rootDir
	}

	splitCmd.Stdout = os.Stdout
	splitCmd.Stderr = os.Stderr
	if err := splitCmd.Run(); err != nil {
		return fmt.Errorf("wa-proto split failed: %w", err)
	}

	if err := sanitizeProtoDefinitions(protoDir); err != nil {
		return fmt.Errorf("failed to sanitize synced proto definitions: %w", err)
	}

	return nil
}

func countProtoMessages(filePath string) (int, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return 0, err
	}
	count := bytes.Count(data, []byte("\nmessage "))
	if bytes.HasPrefix(data, []byte("message ")) {
		count++
	}
	return count, nil
}

func isValidProtoSchema(filePath string) bool {
	count, err := countProtoMessages(filePath)
	return err == nil && count >= 50
}

func validateProtoSchema(filePath string) error {
	count, err := countProtoMessages(filePath)
	if err != nil {
		return fmt.Errorf("reading schema: %w", err)
	}
	if count < 50 {
		return fmt.Errorf("schema contains only %d message definitions (minimum 50 required)", count)
	}
	return nil
}

func fetchRemoteWaProto() (string, func(), error) {
	urls := []string{
		"https://raw.githubusercontent.com/thruqe/WAProto/main/WAProto.proto",
		"https://raw.githubusercontent.com/thruqe/WAProto/v2.3000.1046900546/WAProto.proto",
	}

	for _, u := range urls {
		tmpFile, err := os.CreateTemp("", "WAProto-*.proto")
		if err != nil {
			return "", nil, fmt.Errorf("creating temp file: %w", err)
		}
		cleanup := func() {
			_ = os.Remove(tmpFile.Name())
		}

		resp, err := http.Get(u)
		if err != nil {
			cleanup()
			continue
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			cleanup()
			continue
		}

		_, copyErr := io.Copy(tmpFile, resp.Body)
		resp.Body.Close()
		_ = tmpFile.Close()

		if copyErr != nil {
			cleanup()
			continue
		}

		if isValidProtoSchema(tmpFile.Name()) {
			return tmpFile.Name(), cleanup, nil
		}
		cleanup()
	}

	return "", nil, fmt.Errorf("all remote WAProto.proto sources failed or returned insufficient definitions")
}
