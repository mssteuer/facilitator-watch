package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func syntheticCDPKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal("generate synthetic key:", err)
	}
	return pub, priv
}

func syntheticCDPJSON(t *testing.T, id string, secret []byte) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"id": id, "privateKey": base64.StdEncoding.EncodeToString(secret)})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestCDPJWT(t *testing.T) {
	pub, priv := syntheticCDPKey(t)
	now := time.Unix(1800000000, 123456789)
	for _, secret := range [][]byte{priv.Seed(), priv} {
		key, err := parseCDPKey(syntheticCDPJSON(t, "synthetic-id", secret))
		if err != nil {
			t.Fatal(err)
		}
		previousNonce := ""
		for range 2 {
			req, err := http.NewRequest(http.MethodGet, coinbaseSupportedURL, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header = nil // Successful authorization also supports a nil header.
			if err := key.authorize(req, now); err != nil {
				t.Fatal(err)
			}
			auth := req.Header.Get("Authorization")
			if !strings.HasPrefix(auth, "Bearer ") {
				t.Fatal("missing bearer scheme")
			}
			parts := strings.Split(strings.TrimPrefix(auth, "Bearer "), ".")
			if len(parts) != 3 {
				t.Fatal("invalid JWT shape")
			}
			decoded := make([][]byte, 3)
			for i, part := range parts {
				decoded[i], err = base64.RawURLEncoding.DecodeString(part)
				if err != nil {
					t.Fatal("invalid JWT encoding")
				}
			}
			if !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), decoded[2]) {
				t.Fatal("invalid JWT signature")
			}
			var header map[string]string
			if json.Unmarshal(decoded[0], &header) != nil || len(header) != 4 || header["alg"] != "EdDSA" || header["typ"] != "JWT" || header["kid"] != "synthetic-id" {
				t.Fatal("unexpected JWT header")
			}
			nonce, err := hex.DecodeString(header["nonce"])
			if err != nil || len(nonce) != 16 || header["nonce"] == previousNonce {
				t.Fatal("nonce must be fresh random hex")
			}
			previousNonce = header["nonce"]
			var claims map[string]json.RawMessage
			if json.Unmarshal(decoded[1], &claims) != nil || len(claims) != 5 {
				t.Fatal("unexpected JWT claims")
			}
			expected := map[string]any{"iss": "cdp", "sub": "synthetic-id", "nbf": now.Unix(), "exp": now.Unix() + 120, "uris": []string{"GET api.cdp.coinbase.com/platform/v2/x402/supported"}}
			for name, value := range expected {
				want, _ := json.Marshal(value)
				if string(claims[name]) != string(want) {
					t.Fatalf("unexpected %s claim", name)
				}
			}
		}
	}
}

func TestCDPKeyValidationAndRedaction(t *testing.T) {
	_, priv := syntheticCDPKey(t)
	badPair := append([]byte(nil), priv...)
	badPair[len(badPair)-1] ^= 1
	cases := [][]byte{
		nil, []byte(`null`), []byte(`[]`), []byte(`{`), []byte(`{}`),
		[]byte(`{"id":"synthetic-id"}`), []byte(`{"privateKey":"synthetic-secret"}`),
		[]byte(`{"id":1,"privateKey":"synthetic-secret"}`),
		[]byte(`{"id":"synthetic-id","privateKey":"not base64!"}`),
		syntheticCDPJSON(t, "", priv), syntheticCDPJSON(t, " \t", priv),
		syntheticCDPJSON(t, "synthetic-id", badPair),
		append(syntheticCDPJSON(t, "synthetic-id", priv), []byte(`{}`)...),
		[]byte(strings.Repeat(" ", maxCDPKeyFile+1)),
	}
	for _, size := range []int{0, 1, 31, 33, 63, 65} {
		cases = append(cases, syntheticCDPJSON(t, "synthetic-id", make([]byte, size)))
	}
	for i, raw := range cases {
		key, err := parseCDPKey(raw)
		if err == nil || key != nil {
			t.Fatalf("case %d: accepted invalid key", i)
		}
		if err.Error() != "invalid CDP key" {
			t.Fatalf("case %d: error must not disclose input", i)
		}
	}
	key, err := parseCDPKey(syntheticCDPJSON(t, "synthetic-id", priv))
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{key, *key} {
		for _, format := range []string{"%s", "%v", "%+v", "%#v"} {
			if fmt.Sprintf(format, value) != "cdpKey(redacted)" {
				t.Fatal("key formatting must be redacted")
			}
		}
	}
}

func TestCDPKeyFileBound(t *testing.T) {
	_, priv := syntheticCDPKey(t)
	path := filepath.Join(t.TempDir(), "synthetic-key.json")
	raw := syntheticCDPJSON(t, "synthetic-id", priv)
	raw = append(raw, []byte(strings.Repeat(" ", maxCDPKeyFile-len(raw)))...)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCDPKey(path); err != nil {
		t.Fatal("exact size bound should be accepted:", err)
	}
	if err := os.WriteFile(path, append(raw, ' '), 0600); err != nil {
		t.Fatal(err)
	}
	if key, err := loadCDPKey(path); err == nil || key != nil || err.Error() != "CDP key file unreadable or too large" {
		t.Fatal("oversize file must fail generically")
	}
	if key, err := loadCDPKey(path + "-missing"); err == nil || key != nil || err.Error() != "CDP key file unavailable" {
		t.Fatal("missing file must fail without exposing path")
	}
	if key, err := loadCDPKey(filepath.Dir(path)); err == nil || key != nil {
		t.Fatal("directory must fail")
	}
}

func TestCDPAuthorizationRejectsOtherRequests(t *testing.T) {
	_, priv := syntheticCDPKey(t)
	key, err := parseCDPKey(syntheticCDPJSON(t, "synthetic-id", priv))
	if err != nil {
		t.Fatal(err)
	}
	urls := []string{
		"https://example.invalid/platform/v2/x402/supported",
		"http://api.cdp.coinbase.com/platform/v2/x402/supported",
		"https://api.cdp.coinbase.com:443/platform/v2/x402/supported",
		"https://api.cdp.coinbase.com./platform/v2/x402/supported",
		"https://API.cdp.coinbase.com/platform/v2/x402/supported",
		"https://api.cdp.coinbase.com/platform/v2/x402/verify",
		coinbaseSupportedURL + "/", coinbaseSupportedURL + "?", coinbaseSupportedURL + "?x=1", coinbaseSupportedURL + "#fragment",
		"https://user@api.cdp.coinbase.com/platform/v2/x402/supported",
		"https://api.cdp.coinbase.com/platform/v2/x402/%73upported",
	}
	var requests []*http.Request
	for _, rawURL := range urls {
		req, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		requests = append(requests, req)
	}
	for _, method := range []string{http.MethodPost, http.MethodHead, "get", ""} {
		u, _ := url.Parse(coinbaseSupportedURL)
		requests = append(requests, &http.Request{Method: method, URL: u})
	}
	for _, mutate := range []func(*http.Request){
		func(r *http.Request) { r.Host = "example.invalid" },
		func(r *http.Request) { r.RequestURI = "/elsewhere" },
		func(r *http.Request) { r.URL.RawFragment = "hidden" },
		func(r *http.Request) { r.URL = nil },
	} {
		req, _ := http.NewRequest(http.MethodGet, coinbaseSupportedURL, nil)
		mutate(req)
		requests = append(requests, req)
	}
	for i, req := range requests {
		req.Header = http.Header{"Authorization": {"existing"}, "X-Test": {"unchanged"}}
		before := req.Header.Clone()
		if err := key.authorize(req, time.Now()); err == nil {
			t.Fatalf("case %d: unauthorized request accepted", i)
		}
		if !reflect.DeepEqual(req.Header, before) {
			t.Fatalf("case %d: rejected request headers changed", i)
		}
	}
	if err := key.authorize(nil, time.Now()); err == nil {
		t.Fatal("nil request accepted")
	}
	for _, invalid := range []*cdpKey{nil, {}, {id: "synthetic-id"}} {
		req, _ := http.NewRequest(http.MethodGet, coinbaseSupportedURL, nil)
		req.Header = nil
		if err := invalid.authorize(req, time.Now()); err == nil || req.Header != nil {
			t.Fatal("invalid key must fail without mutating headers")
		}
	}
}
