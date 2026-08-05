package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

type accountCredential struct {
	Password     string `json:"password,omitempty"`
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	TokenExpiry  string `json:"token_expiry,omitempty"`
}

type credentialVault struct{ aead cipher.AEAD }

func loadCredentialVault() (*credentialVault, error) {
	encoded := os.Getenv("BESPOKE_CALENDAR_KEY")
	if encoded == "" {
		// Instances upgrading from Mail can deliberately share the encryption
		// key without sharing either app's database or credentials.
		encoded = os.Getenv("BESPOKE_MAIL_KEY")
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(key) != 32 {
		return nil, errors.New("BESPOKE_CALENDAR_KEY must be a base64-encoded 32-byte key")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &credentialVault{aead: aead}, nil
}

func (v *credentialVault) Seal(value accountCredential) ([]byte, []byte, error) {
	if v == nil {
		return nil, nil, errors.New("calendar credential encryption is unavailable")
	}
	plain, err := json.Marshal(value)
	if err != nil {
		return nil, nil, err
	}
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	return v.aead.Seal(nil, nonce, plain, nil), nonce, nil
}

func (v *credentialVault) Open(ciphertext, nonce []byte) (accountCredential, error) {
	if v == nil {
		return accountCredential{}, errors.New("calendar credential encryption is unavailable")
	}
	plain, err := v.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return accountCredential{}, fmt.Errorf("decrypt calendar credentials: %w", err)
	}
	var value accountCredential
	if err := json.Unmarshal(plain, &value); err != nil {
		return accountCredential{}, errors.New("decode calendar credentials")
	}
	return value, nil
}
