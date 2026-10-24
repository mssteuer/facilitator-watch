package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	coinbaseSupportedURL = "https://api.cdp.coinbase.com/platform/v2/x402/supported"
	maxCDPKeyFile        = 16 << 10
)

// cdpKey never exposes its identifier or signing material through formatting.
type cdpKey struct {
	id   string
	priv ed25519.PrivateKey
}

func (k cdpKey) String() string   { return "cdpKey(redacted)" }
func (k cdpKey) GoString() string { return k.String() }

func loadCDPKey(path string) (*cdpKey, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("CDP key file unavailable")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxCDPKeyFile+1))
	defer clear(raw)
	if err != nil || len(raw) > maxCDPKeyFile {
		return nil, errors.New("CDP key file unreadable or too large")
	}
	return parseCDPKey(raw)
}

// parseCDPKey accepts CDP's standard-base64 Ed25519 seed or seed||public format.
func parseCDPKey(raw []byte) (*cdpKey, error) {
	invalid := errors.New("invalid CDP key")
	if len(raw) > maxCDPKeyFile {
		return nil, invalid
	}
	var fields struct {
		ID         string `json:"id"`
		PrivateKey string `json:"privateKey"`
	}
	if json.Unmarshal(raw, &fields) != nil || strings.TrimSpace(fields.ID) == "" || fields.PrivateKey == "" {
		return nil, invalid
	}
	secret, err := base64.StdEncoding.Strict().DecodeString(fields.PrivateKey)
	defer clear(secret)
	if err != nil || (len(secret) != ed25519.SeedSize && len(secret) != ed25519.PrivateKeySize) {
		return nil, invalid
	}
	priv := ed25519.NewKeyFromSeed(secret[:ed25519.SeedSize])
	if len(secret) == ed25519.PrivateKeySize && !bytes.Equal(priv, secret) {
		clear(priv)
		return nil, invalid
	}
	return &cdpKey{id: fields.ID, priv: priv}, nil
}

// authorize only signs the one approved read. The caller must also disable
// redirects: a previously authorized request must never be forwarded elsewhere.
// No request fields are changed on failure, including existing Authorization.
func (k *cdpKey) authorize(req *http.Request, now time.Time) error {
	if req == nil || req.URL == nil || req.Method != http.MethodGet ||
		req.URL.String() != coinbaseSupportedURL || req.URL.User != nil ||
		req.URL.RawQuery != "" || req.URL.ForceQuery || req.URL.Fragment != "" || req.URL.RawFragment != "" ||
		req.URL.RawPath != "" || req.URL.Opaque != "" || req.RequestURI != "" ||
		(req.Host != "" && req.Host != "api.cdp.coinbase.com") {
		return errors.New("CDP authorization requires the fixed supported GET")
	}
	if k == nil || k.id == "" || len(k.priv) != ed25519.PrivateKeySize {
		return errors.New("invalid CDP key")
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return errors.New("CDP authorization unavailable")
	}
	header, err := json.Marshal(map[string]any{
		"alg": "EdDSA", "typ": "JWT", "kid": k.id, "nonce": hex.EncodeToString(nonce),
	})
	if err != nil {
		return errors.New("CDP authorization unavailable")
	}
	claims, err := json.Marshal(map[string]any{
		"iss": "cdp", "sub": k.id, "nbf": now.Unix(), "exp": now.Add(120 * time.Second).Unix(),
		"uris": []string{"GET api.cdp.coinbase.com/platform/v2/x402/supported"},
	})
	if err != nil {
		return errors.New("CDP authorization unavailable")
	}
	encode := base64.RawURLEncoding.EncodeToString
	signing := encode(header) + "." + encode(claims)
	token := signing + "." + encode(ed25519.Sign(k.priv, []byte(signing)))
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}
