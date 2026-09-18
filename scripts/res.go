package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func runRes(args []string) error {
	rootDir, err := findRepoRoot()
	if err != nil {
		return fmt.Errorf("failed to locate repo root: %w", err)
	}

	now := time.Now()
	productVersion := ""
	if len(args) > 0 && strings.TrimSpace(args[0]) != "" {
		productVersion = strings.TrimPrefix(strings.TrimSpace(args[0]), "v")
	}
	if productVersion == "" {
		yy := now.Year() % 100
		mm := int(now.Month())
		sha := getGitShortSHA(rootDir)
		productVersion = fmt.Sprintf("%02d.%02d.%s", yy, mm, sha)
	}

	// Windows PE --file-version must be 4 numeric components (e.g. 26.9.0.0)
	parts := strings.Split(productVersion, ".")
	var yr, mo int
	if len(parts) >= 2 {
		yr, _ = strconv.Atoi(parts[0])
		mo, _ = strconv.Atoi(parts[1])
	}
	if yr == 0 {
		yr = now.Year() % 100
	}
	if mo == 0 {
		mo = int(now.Month())
	}
	fileVersion := fmt.Sprintf("%d.%d.0.0", yr, mo)

	iconPath := filepath.Join(rootDir, "assets", "logo.png")
	if _, err := os.Stat(iconPath); err != nil {
		return fmt.Errorf("icon not found at %s: %w", iconPath, err)
	}

	cmdDir := filepath.Join(rootDir, "cmd")
	year := time.Now().Year()

	fmt.Printf("Generating binary resources (Product Version: %s, File Version: %s, Icon: %s)...\n", productVersion, fileVersion, filepath.Base(iconPath))

	cmd := exec.Command("go", "run", "github.com/tc-hib/go-winres@latest", "simply",
		"--icon", iconPath,
		"--manifest", "cli",
		"--product-name", "WhatsRook",
		"--file-description", "WhatsApp Automation Client",
		"--copyright", fmt.Sprintf("Copyright © %d Thruqe", year),
		"--original-filename", "whatsrook.exe",
		"--file-version", fileVersion,
		"--product-version", productVersion,
		"--arch", "amd64,arm64,386,arm",
	)
	// Ensure the go-winres invocation uses the native host toolchain
	var hostEnv []string
	for _, env := range os.Environ() {
		if strings.HasPrefix(env, "GOOS=") || strings.HasPrefix(env, "GOARCH=") {
			continue
		}
		hostEnv = append(hostEnv, env)
	}
	hostEnv = append(hostEnv, "GOOS=", "GOARCH=")
	cmd.Env = hostEnv
	cmd.Dir = cmdDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go-winres failed: %w", err)
	}

	fmt.Println("✓ Windows PE resources generated successfully.")
	return nil
}
