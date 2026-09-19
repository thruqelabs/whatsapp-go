package main

import (
	"bytes"
	"compress/flate"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// MasterKey must match the 32-byte key defined in src/lib/crypto.ts
var MasterKey = []byte("ThruqeCustomWABotMasterKey_2026!")

// WebSessionPayload matches the JSON shape serialized in the frontend
type WebSessionPayload struct {
	Phone      string `json:"phone"`
	Business   bool   `json:"business"`
	Auth       string `json:"auth"`
	Client     string `json:"client"`
	DB         string `json:"db"`
	AutoUpdate bool   `json:"autoupdate"`
	Verbose    bool   `json:"verbose"`
}

// DecryptSessionID decrypts and unpacks a compact "SE_ID:..." token
func DecryptSessionID(token string) (*WebSessionPayload, error) {
	if !strings.HasPrefix(token, "SE_ID:") {
		return nil, errors.New("invalid session token: expected SE_ID: prefix")
	}

	rawPayload := strings.TrimPrefix(token, "SE_ID:")
	data, err := base64.RawURLEncoding.DecodeString(rawPayload)
	if err != nil {
		return nil, err
	}

	// 12-byte Nonce + minimum 16-byte GCM Tag = 28 bytes minimum
	if len(data) < 28 {
		return nil, errors.New("ciphertext payload too short")
	}

	block, err := aes.NewCipher(MasterKey)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonceSize := gcm.NonceSize() // 12
	nonce, ciphertext := data[:nonceSize], data[nonceSize:]

	compressed, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, errors.New("authentication verification failed or corrupt token")
	}

	// Decompress raw Deflate stream
	reader := flate.NewReader(bytes.NewReader(compressed))
	defer reader.Close()

	decompressed, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}

	var payload WebSessionPayload
	if err := json.Unmarshal(decompressed, &payload); err != nil {
		return nil, err
	}

	return &payload, nil
}
