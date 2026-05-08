// Rate limiting, file-size caps, MCP concurrency control, and the friendly
// upgrade messages we return when free-tier limits are hit. Knobs are env
// vars with sensible defaults; everything is per-IP except the MCP
// concurrency semaphore (which is global).
package server

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ── Defaults ──────────────────────────────────────────────────────────────────

const (
	defaultMaxUploadBytes    int64 = 100 * 1024 * 1024 // 100 MB
	defaultPresignsPerHour   int   = 5
	defaultMCPCallsPerHour   int   = 5
	defaultMCPConcurrency    int   = 10

	earlyAccessEmail = "api@subtitlesking.com"
	selfHostURL      = "https://www.subtitlesking.com/self-host"
	pricingURL       = "https://www.subtitlesking.com/pricing"
)

func envInt(name string, fallback int) int {
	if s := os.Getenv(name); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

func envBytes(name string, fallback int64) int64 {
	if s := os.Getenv(name); s != "" {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

var (
	// All env-var names are prefixed with SUBTITLESKING_ to avoid collisions
	// on the shared VPS where multiple services live in the same systemd
	// environment plane.
	maxUploadBytes  = envBytes("SUBTITLESKING_MAX_UPLOAD_BYTES", defaultMaxUploadBytes)
	presignsPerHour = envInt("SUBTITLESKING_PRESIGN_LIMIT_PER_HOUR", defaultPresignsPerHour)
	mcpCallsPerHour = envInt("SUBTITLESKING_MCP_LIMIT_PER_HOUR", defaultMCPCallsPerHour)
	mcpConcurrency  = envInt("SUBTITLESKING_MCP_CONCURRENCY", defaultMCPConcurrency)

	presignLimiter   = newRateLimiter(time.Hour, presignsPerHour)
	mcpUploadLimiter = newRateLimiter(time.Hour, mcpCallsPerHour)
	mcpSlots         = make(chan struct{}, mcpConcurrency)
)

// ── Per-IP rate limiter (token bucket via sliding window) ─────────────────────

type rateLimiter struct {
	mu      sync.Mutex
	history map[string][]time.Time
	window  time.Duration
	max     int
}

func newRateLimiter(window time.Duration, max int) *rateLimiter {
	rl := &rateLimiter{
		history: make(map[string][]time.Time),
		window:  window,
		max:     max,
	}
	go rl.gc()
	return rl
}

// allow records an attempt from key and returns (allowed, retryAfter).
// retryAfter is the time until the oldest in-window attempt expires.
func (rl *rateLimiter) allow(key string) (bool, time.Duration) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-rl.window)
	times := rl.history[key]

	n := 0
	for _, t := range times {
		if t.After(cutoff) {
			times[n] = t
			n++
		}
	}
	times = times[:n]

	if len(times) >= rl.max {
		retry := times[0].Add(rl.window).Sub(now)
		rl.history[key] = times
		return false, retry
	}

	times = append(times, now)
	rl.history[key] = times
	return true, 0
}

func (rl *rateLimiter) gc() {
	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		rl.mu.Lock()
		cutoff := time.Now().Add(-rl.window)
		for key, times := range rl.history {
			n := 0
			for _, t := range times {
				if t.After(cutoff) {
					times[n] = t
					n++
				}
			}
			if n == 0 {
				delete(rl.history, key)
			} else {
				rl.history[key] = times[:n]
			}
		}
		rl.mu.Unlock()
	}
}

// ── IP extraction ─────────────────────────────────────────────────────────────

// clientIP returns the requester's IP. Honors X-Real-IP and X-Forwarded-For
// because nginx sits in front of this server in production.
func clientIP(r *http.Request) string {
	if ip := r.Header.Get("X-Real-IP"); ip != "" {
		return ip
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ── Friendly error responses ──────────────────────────────────────────────────

type upgradeOptions struct {
	EarlyAccessEmail string `json:"earlyAccessEmail"`
	SelfHost         string `json:"selfHost"`
	Pricing          string `json:"pricing"`
}

type errorResponse struct {
	Error             string         `json:"error"`
	Message           string         `json:"message"`
	Limit             string         `json:"limit,omitempty"`
	MaxBytes          int64          `json:"maxBytes,omitempty"`
	RetryAfterSeconds int            `json:"retryAfterSeconds,omitempty"`
	Options           upgradeOptions `json:"options"`
}

func defaultUpgradeOptions() upgradeOptions {
	return upgradeOptions{
		EarlyAccessEmail: earlyAccessEmail,
		SelfHost:         selfHostURL,
		Pricing:          pricingURL,
	}
}

func writeRateLimitError(w http.ResponseWriter, retryAfter time.Duration) {
	secs := int(retryAfter.Seconds()) + 1
	mins := secs / 60
	if mins < 1 {
		mins = 1
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", fmt.Sprintf("%d", secs))
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(errorResponse{
		Error: "rate_limit",
		Message: fmt.Sprintf(
			"You've hit the free-tier limit of %d uploads per hour from this IP. Try again in about %d minute(s). We're launching a Pro plan with higher limits — email %s for early access. Or run our open-source MCP locally for free, unlimited use: %s",
			presignsPerHour, mins, earlyAccessEmail, selfHostURL,
		),
		Limit:             fmt.Sprintf("%d / hour / IP", presignsPerHour),
		RetryAfterSeconds: secs,
		Options:           defaultUpgradeOptions(),
	})
}

func writeFileTooLargeError(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusRequestEntityTooLarge)
	mb := maxUploadBytes / (1024 * 1024)
	_ = json.NewEncoder(w).Encode(errorResponse{
		Error: "file_too_large",
		Message: fmt.Sprintf(
			"Your video exceeds the %d MB free-tier limit. We're launching a Pro plan with multi-GB uploads — email %s for early access before public launch. Or self-host our MCP locally for unlimited file sizes (free): %s",
			mb, earlyAccessEmail, selfHostURL,
		),
		MaxBytes: maxUploadBytes,
		Options:  defaultUpgradeOptions(),
	})
}

// mcpFriendlyMessage returns a plain-text message suitable for an MCP
// JSON-RPC error so AI agents can relay it directly to the user.
func mcpFriendlyMessage(reason string, retryAfter time.Duration) string {
	switch reason {
	case "rate_limit":
		mins := int(retryAfter.Minutes()) + 1
		return fmt.Sprintf(
			"Rate limit hit (%d uploads/hour per IP on the hosted MCP). Try again in about %d min, email %s for Pro early access, or run the MCP locally for free unlimited use: %s",
			mcpCallsPerHour, mins, earlyAccessEmail, selfHostURL,
		)
	case "file_too_large":
		mb := maxUploadBytes / (1024 * 1024)
		return fmt.Sprintf(
			"Video exceeds the %d MB free-tier limit. Email %s for Pro early access (multi-GB uploads coming), or run the MCP locally for unlimited file sizes: %s",
			mb, earlyAccessEmail, selfHostURL,
		)
	case "concurrency":
		return fmt.Sprintf(
			"Hosted MCP is busy (max %d concurrent calls). Wait a few seconds and retry, or run the MCP locally for unlimited concurrency: %s",
			mcpConcurrency, selfHostURL,
		)
	}
	return ""
}

// ── MCP concurrency gate ──────────────────────────────────────────────────────

// acquireMCPSlot tries to grab a server-wide MCP slot. Returns false if the
// server is at concurrency cap. Caller must releaseMCPSlot on success.
func acquireMCPSlot() bool {
	select {
	case mcpSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func releaseMCPSlot() {
	select {
	case <-mcpSlots:
	default:
	}
}
