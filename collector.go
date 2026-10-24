package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	pollInterval  = 30 * time.Minute
	pollTimeout   = 8 * time.Second
	maxResponse   = 1 << 20
	maxRetryAfter = 6 * time.Hour
	userAgent     = "FacilitatorWatch/1.0 (passive GET /supported monitor; https://facilitators.shveik.dev)"
)

func parseSupported(raw []byte) (Supported, error) {
	var s Supported
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return s, errors.New("invalid /supported JSON object")
	}
	var rawKinds []map[string]json.RawMessage
	if err := json.Unmarshal(fields["kinds"], &rawKinds); err != nil || rawKinds == nil {
		return s, errors.New("invalid /supported: required kinds[] objects")
	}
	if len(rawKinds) > 256 {
		return s, errors.New("/supported exceeds 256 capability entries")
	}
	s.Kinds = make([]SupportedKind, 0, len(rawKinds))
	v2 := false
	for _, fields := range rawKinds {
		var k SupportedKind
		if json.Unmarshal(fields["x402Version"], &k.X402Version) != nil || json.Unmarshal(fields["scheme"], &k.Scheme) != nil || json.Unmarshal(fields["network"], &k.Network) != nil {
			return s, errors.New("kind requires exact x402Version, scheme and network field names/types")
		}
		k.Extra = fields["extra"]
		if k.X402Version < 1 || strings.TrimSpace(k.Scheme) == "" || strings.TrimSpace(k.Network) == "" {
			return s, errors.New("invalid /supported kind: version, scheme and network required")
		}
		v2 = v2 || k.X402Version == 2
		s.Kinds = append(s.Kinds, k)
	}
	if raw, ok := fields["extensions"]; ok {
		if err := json.Unmarshal(raw, &s.Extensions); err != nil || s.Extensions == nil {
			return s, errors.New("invalid extensions[]")
		}
	}
	if raw, ok := fields["signers"]; ok {
		var rawSigners map[string]json.RawMessage
		if err := json.Unmarshal(raw, &rawSigners); err != nil || rawSigners == nil {
			return s, errors.New("invalid signers{}")
		}
		signers := make(map[string][]string, len(rawSigners))
		for network, rawValues := range rawSigners {
			if strings.TrimSpace(network) == "" {
				return s, errors.New("invalid signers{} network key")
			}
			var values []json.RawMessage
			if err := json.Unmarshal(rawValues, &values); err != nil || values == nil {
				return s, errors.New("invalid signers{} values")
			}
			parsed := make([]string, len(values))
			for i, value := range values {
				if err := json.Unmarshal(value, &parsed[i]); err != nil || strings.TrimSpace(parsed[i]) == "" {
					return s, errors.New("invalid signers{} signer")
				}
			}
			signers[network] = parsed
		}
		s.Signers = &signers
	}
	if v2 && (s.Extensions == nil || s.Signers == nil) {
		return s, errors.New("invalid v2 /supported: extensions[] and signers{} required")
	}
	if len(s.Kinds)+len(s.Extensions) > 256 {
		return s, errors.New("/supported exceeds 256 capability entries")
	}
	for _, x := range s.Extensions {
		if strings.TrimSpace(x) == "" {
			return s, errors.New("invalid empty extension")
		}
	}
	return s, nil
}

func newPollClient() *http.Client {
	// Explicit transport avoids proxy environment variables, cookies and
	// shared DefaultTransport mutations. TLS certificates are always verified.
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            (&net.Dialer{Timeout: pollTimeout}).DialContext,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:    pollTimeout,
		ResponseHeaderTimeout:  pollTimeout,
		MaxResponseHeaderBytes: 64 << 10,
		MaxConnsPerHost:        1,
		MaxIdleConns:           2,
		IdleConnTimeout:        time.Minute,
		// Avoid automatic GET retries on a reused connection: one request
		// per scheduled check makes collection frequency predictable.
		DisableKeepAlives: true,
	}
	return &http.Client{Transport: transport, Timeout: pollTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func collect(ctx context.Context, target FacilitatorTarget, client *http.Client, keys ...*cdpKey) (pollResult, error) {
	fixed, ok := targetByID(target.ID)
	if !ok || fixed != target {
		return pollResult{}, errors.New("unapproved target")
	}
	var key *cdpKey
	if len(keys) > 0 {
		key = keys[0]
	}
	if target.ID == "coinbase-cdp" && key == nil {
		return pollResult{}, errors.New("Coinbase CDP collection requires configuration")
	}
	ctx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()
	start := time.Now()
	r := pollResult{}
	finish := func() pollResult { r.At = time.Now().UTC(); r.MS = int(time.Since(start).Milliseconds()); return r }
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.Endpoint+"/supported", nil)
	if err != nil {
		return r, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if target.ID == "coinbase-cdp" {
		if req.URL.String() != coinbaseSupportedURL {
			return pollResult{}, errors.New("unapproved Coinbase CDP target")
		}
		if err := key.authorize(req, time.Now()); err != nil {
			return pollResult{}, errors.New("Coinbase CDP authorization failed")
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		r.Error = "GET /supported failed"
		return finish(), nil
	}
	defer resp.Body.Close()
	r.Reachable = true
	code := resp.StatusCode
	r.HTTPStatus = &code
	if code == http.StatusTooManyRequests || code == http.StatusServiceUnavailable {
		r.RetryAfter = retryAfter(resp.Header.Get("Retry-After"), time.Now())
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	oversized := len(body) > maxResponse
	if oversized {
		body = body[:maxResponse]
	}
	// A provider error must not persist a reflected bearer token.
	if token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "); token != "" {
		body = []byte(strings.ReplaceAll(string(body), token, "[redacted]"))
	}
	if oversized {
		r.Raw = body
		r.Error = "response exceeds 1 MiB limit; stored body truncated"
	} else {
		r.Raw = body
		switch {
		case err != nil:
			r.Error = "response read failed"
		case code < 200 || code >= 300:
			r.Error = fmt.Sprintf("HTTP %d; redirects are disabled", code)
		default:
			if _, err = parseSupported(body); err != nil {
				r.Error = err.Error()
			} else {
				r.Valid = true
			}
		}
	}
	return finish(), nil
}

func retryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		if seconds >= int64(maxRetryAfter/time.Second) {
			return maxRetryAfter
		}
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		delay := at.Sub(now)
		if delay > maxRetryAfter {
			return maxRetryAfter
		}
		if delay > 0 {
			return delay
		}
	}
	return 0
}

func nextPollDelay(failures int, suggested time.Duration) time.Duration {
	if failures == 0 {
		return pollInterval
	}
	if failures > 6 {
		failures = 6
	}
	delay := time.Minute * time.Duration(1<<uint(failures-1))
	if delay > pollInterval {
		delay = pollInterval
	}
	if suggested > maxRetryAfter {
		suggested = maxRetryAfter
	}
	if suggested > delay {
		delay = suggested
	}
	return delay
}

// One scheduler per enabled target, with a global two-request semaphore.
// Waiting for a slot is cancellation-aware; the request timeout starts afterward.
// Each starts immediately, then waits after completion. Cancel and join before
// closing SQLite so shutdown cannot race a collector write.
// In once mode each worker exits after its first attempt, even on failure.
func runCollector(ctx context.Context, s *liveStore, client *http.Client, once bool, keys ...*cdpKey) {
	var wg sync.WaitGroup
	slots := make(chan struct{}, 2)
	var key *cdpKey
	if len(keys) > 0 {
		key = keys[0]
	}
	for _, target := range targets() {
		if target.ID == "coinbase-cdp" && key == nil {
			continue
		}
		wg.Add(1)
		go func(target FacilitatorTarget) {
			defer wg.Done()
			failures := 0
			for ctx.Err() == nil {
				select {
				case <-ctx.Done():
					return
				case slots <- struct{}{}:
				}
				if ctx.Err() != nil {
					<-slots
					return
				}
				r, err := collect(ctx, target, client, key)
				<-slots
				if err != nil {
					log.Printf("collector %s: %v", target.ID, err)
					return
				}
				if ctx.Err() != nil {
					return
				}
				err = s.record(ctx, target, r)
				if err != nil {
					log.Printf("store check %s: %v", target.ID, err)
				}
				if once {
					return
				}
				if r.Valid && err == nil {
					failures = 0
				} else if failures < 6 {
					failures++
				}
				timer := time.NewTimer(nextPollDelay(failures, r.RetryAfter))
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
		}(target)
	}
	wg.Wait()
}
