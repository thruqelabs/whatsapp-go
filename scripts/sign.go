package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

func runSign(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: go run ./scripts sign <path-to-windows-pe-binary>")
	}
	targetBinary := args[0]
	if !filepath.IsAbs(targetBinary) {
		rootDir, err := findRepoRoot()
		if err == nil {
			if _, err := os.Stat(filepath.Join(rootDir, targetBinary)); err == nil {
				targetBinary = filepath.Join(rootDir, targetBinary)
			} else if _, err := os.Stat(targetBinary); err == nil {
				if abs, errAbs := filepath.Abs(targetBinary); errAbs == nil {
					targetBinary = abs
				}
			}
		}
	}

	fi, err := os.Stat(targetBinary)
	if err != nil {
		return fmt.Errorf("binary not found at %s: %w", targetBinary, err)
	}
	if fi.IsDir() {
		return fmt.Errorf("%s is a directory, not a binary", targetBinary)
	}

	fmt.Printf("Digitally signing Windows executable %s...\n", filepath.Base(targetBinary))

	// Locate signing tool: osslsigncode (cross-platform / Linux) or signtool (Windows)
	osslPath, errOssl := exec.LookPath("osslsigncode")
	signtoolPath, errSigntool := exec.LookPath("signtool.exe")
	if errSigntool != nil {
		signtoolPath, errSigntool = exec.LookPath("signtool")
	}

	if errOssl != nil && errSigntool != nil {
		return fmt.Errorf("no code-signing tool found (install 'osslsigncode' on Linux/macOS or 'signtool' on Windows)")
	}

	// Generate random crypto key & self-signed code-signing certificate
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return fmt.Errorf("failed to generate random RSA key: %w", err)
	}

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return fmt.Errorf("failed to generate random serial: %w", err)
	}

	now := time.Now()
	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:   "WhatsRook Authenticode Signing",
			Organization: []string{"ThruqeLabs"},
			Country:      []string{"US"},
		},
		NotBefore:             now.Add(-1 * time.Hour),
		NotAfter:              now.Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &privKey.PublicKey, privKey)
	if err != nil {
		return fmt.Errorf("failed to create self-signed certificate: %w", err)
	}

	tmpDir, err := os.MkdirTemp("", "whatsrook-sign-*")
	if err != nil {
		return fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	keyPath := filepath.Join(tmpDir, "signing.key")
	certPath := filepath.Join(tmpDir, "signing.crt")

	keyOut, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("failed to create key file: %w", err)
	}
	if err := pem.Encode(keyOut, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privKey)}); err != nil {
		_ = keyOut.Close()
		return fmt.Errorf("failed to encode private key: %w", err)
	}
	_ = keyOut.Close()

	certOut, err := os.OpenFile(certPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("failed to create cert file: %w", err)
	}
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: certDER}); err != nil {
		_ = certOut.Close()
		return fmt.Errorf("failed to encode certificate: %w", err)
	}
	_ = certOut.Close()

	signedOutput := targetBinary + ".signed"
	_ = os.Remove(signedOutput)

	if errOssl == nil {
		// Use osslsigncode
		signCmd := exec.Command(osslPath, "sign",
			"-certs", certPath,
			"-key", keyPath,
			"-h", "sha256",
			"-n", "WhatsRook",
			"-i", "https://github.com/thruqelabs/whatsapp-go",
			"-in", targetBinary,
			"-out", signedOutput,
		)
		signCmd.Stdout = os.Stdout
		signCmd.Stderr = os.Stderr
		if err := signCmd.Run(); err != nil {
			_ = os.Remove(signedOutput)
			return fmt.Errorf("osslsigncode failed: %w", err)
		}
	} else if runtime.GOOS == "windows" {
		// Fallback to signtool on Windows if available
		signCmd := exec.Command(signtoolPath, "sign",
			"/a",
			"/fd", "sha256",
			"/d", "WhatsRook",
			"/du", "https://github.com/thruqelabs/whatsapp-go",
			targetBinary,
		)
		signCmd.Stdout = os.Stdout
		signCmd.Stderr = os.Stderr
		if err := signCmd.Run(); err != nil {
			return fmt.Errorf("signtool failed: %w", err)
		}
		fmt.Printf("✓ Successfully digitally signed %s with signtool.\n", filepath.Base(targetBinary))
		return nil
	}

	if err := os.Rename(signedOutput, targetBinary); err != nil {
		_ = os.Remove(signedOutput)
		return fmt.Errorf("failed to replace binary with signed output: %w", err)
	}

	fmt.Printf("✓ Successfully digitally signed %s with SHA-256 Authenticode signature.\n", filepath.Base(targetBinary))
	return nil
}
