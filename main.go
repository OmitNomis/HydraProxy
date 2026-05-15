package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"strconv"
	"sync/atomic"
	"time"
)

// ─────────────────────────────────────────────
// Key Pool
// ─────────────────────────────────────────────

type KeyPool struct {
	keys      []string
	cooldowns []time.Time
	disabled  []bool
	mu        sync.Mutex
	counter   uint64
}

func NewKeyPool(keys []string) *KeyPool {
	return &KeyPool{
		keys:      keys,
		cooldowns: make([]time.Time, len(keys)),
		disabled:  make([]bool, len(keys)),
	}
}

// TimeUntilAvailable returns how long until the soonest key becomes available.
// Returns 0 if any key is already available.
func (p *KeyPool) TimeUntilAvailable() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	var soonest time.Duration = -1
	for i, cd := range p.cooldowns {
		if p.disabled[i] {
			continue
		}
		if now.After(cd) {
			return 0 // a key is ready right now
		}
		wait := cd.Sub(now)
		if soonest < 0 || wait < soonest {
			soonest = wait
		}
	}
	return soonest
}

// Next returns the index + key for the next available key using round-robin.
// A key is "available" if its cooldown has expired.
func (p *KeyPool) Next() (int, string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	n := len(p.keys)
	start := int(atomic.AddUint64(&p.counter, 1)-1) % n

	for i := 0; i < n; i++ {
		idx := (start + i) % n
		if !p.disabled[idx] && time.Now().After(p.cooldowns[idx]) {
			return idx, p.keys[idx], true
		}
	}
	return -1, "", false
}

// Cooldown puts a key on cooldown for the specified duration.
func (p *KeyPool) Cooldown(idx int, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	until := time.Now().Add(d)
	if p.cooldowns[idx].Before(until) {
		p.cooldowns[idx] = until
	}
	log.Printf("🧊 Key [%d] on cooldown for %s", idx, d)
}

// Disable permanently removes a key from rotation (e.g. after a 401/403).
func (p *KeyPool) Disable(idx int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.disabled[idx] = true
}

// ActiveCount returns the number of keys that are not permanently disabled.
func (p *KeyPool) ActiveCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	count := 0
	for i := range p.keys {
		if !p.disabled[i] {
			count++
		}
	}
	return count
}

// Status returns a snapshot for logging.
func (p *KeyPool) Status() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	parts := make([]string, len(p.keys))
	for i, cd := range p.cooldowns {
		if p.disabled[i] {
			parts[i] = fmt.Sprintf("[%d]:disabled", i)
		} else if now.After(cd) {
			parts[i] = fmt.Sprintf("[%d]:ready", i)
		} else {
			parts[i] = fmt.Sprintf("[%d]:cooling(%.0fs)", i, cd.Sub(now).Seconds())
		}
	}
	return strings.Join(parts, "  ")
}

// ─────────────────────────────────────────────
// Config
// ─────────────────────────────────────────────

type Config struct {
	TargetBase  string
	Port        string
	MaxRetries  int
	CooldownSec int
}

func loadConfig() (Config, *KeyPool) {
	cfg := Config{
		TargetBase:  strings.TrimRight(getenv("TARGET_BASE_URL", "https://integrate.api.nvidia.com/v1"), "/"),
		Port:        getenv("PORT", "3000"),
		MaxRetries:  10,
		CooldownSec: 60,
	}

	raw := getenv("API_KEYS", "")
	if raw == "" {
		log.Fatal("❌ API_KEYS is required. Set it in your .env or environment.")
	}

	var keys []string
	for _, k := range strings.Split(raw, ",") {
		k = strings.TrimSpace(k)
		if k != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		log.Fatal("❌ No valid API keys found in API_KEYS.")
	}

	return cfg, NewKeyPool(keys)
}

func getenv(key, fallback string) string {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	return v
}

// ─────────────────────────────────────────────
// Proxy Handler
// ─────────────────────────────────────────────

func makeHandler(pool *KeyPool, cfg Config) http.HandlerFunc {
	client := &http.Client{
		Timeout: 0, // streaming — no timeout
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	return func(w http.ResponseWriter, r *http.Request) {
		// Read body once; we may need to replay it on retry.
		var bodyBytes []byte
		if r.Body != nil {
			var err error
			bodyBytes, err = io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "failed to read request body", http.StatusBadRequest)
				return
			}
			r.Body.Close()
		}

		// Cline sends /v1/chat/completions; if our base already ends in /v1,
		// strip the leading /v1 to avoid /v1/v1/chat/completions.
		incomingPath := r.URL.Path
		if strings.HasSuffix(cfg.TargetBase, "/v1") && strings.HasPrefix(incomingPath, "/v1") {
			incomingPath = incomingPath[3:]
		}
		if r.URL.RawQuery != "" {
			incomingPath += "?" + r.URL.RawQuery
		}
		target := cfg.TargetBase + incomingPath

		log.Printf("→ %s %s  (body: %d bytes)", r.Method, target, len(bodyBytes))
		log.Printf("  headers: %v", r.Header)
		if len(bodyBytes) > 0 && len(bodyBytes) < 2000 {
			log.Printf("  body: %s", string(bodyBytes))
		}

		for attempt := 0; attempt < cfg.MaxRetries; attempt++ {
			idx, key, ok := pool.Next()
			if !ok {
				wait := pool.TimeUntilAvailable()
				log.Printf("⏳ All keys cooling. Waiting %s for next available key… (attempt %d/%d)", wait.Round(time.Second), attempt+1, cfg.MaxRetries)
				time.Sleep(wait + 500*time.Millisecond) // +500ms buffer
				continue
			}

			log.Printf("  ↑ attempt %d: key[%d] → %s", attempt+1, idx, target)

			upstream, err := http.NewRequest(r.Method, target, bytes.NewReader(bodyBytes))
			if err != nil {
				http.Error(w, "proxy: failed to build upstream request", http.StatusInternalServerError)
				return
			}

			// Copy original headers, overwrite Authorization.
			for k, vals := range r.Header {
				for _, v := range vals {
					upstream.Header.Add(k, v)
				}
			}
			upstream.Header.Set("Authorization", "Bearer "+key)

			resp, err := client.Do(upstream)
			if err != nil {
				log.Printf("⚠️  Key [%d] network error: %v", idx, err)
				pool.Cooldown(idx, time.Duration(cfg.CooldownSec)*time.Second)
				continue
			}

			if resp.StatusCode == http.StatusTooManyRequests {
				retryBody, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				// Respect Retry-After if upstream sends it
				cooldown := time.Duration(cfg.CooldownSec) * time.Second
				if ra := resp.Header.Get("Retry-After"); ra != "" {
					if secs, err := strconv.Atoi(ra); err == nil {
						cooldown = time.Duration(secs+2) * time.Second // +2s buffer
					}
				}
				log.Printf("🚫 Key [%d] hit 429. Cooldown: %s", idx, cooldown)
				log.Printf("   429 body: %s", string(retryBody))
				log.Printf("   429 headers: %v", resp.Header)
				log.Printf("   Pool: %s", pool.Status())
				pool.Cooldown(idx, cooldown)
				continue
			}

			if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
				// Key is invalid, revoked, or expired — remove it from rotation permanently
				errBody, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				log.Printf("🔑 Key [%d] rejected with %d (invalid/revoked). Removing from pool permanently.", idx, resp.StatusCode)
				log.Printf("   body: %s", string(errBody))
				pool.Disable(idx)
				if pool.ActiveCount() == 0 {
					log.Printf("💀 No valid keys remaining!")
					http.Error(w, "hydra-proxy: all keys are invalid or revoked", http.StatusServiceUnavailable)
					return
				}
				continue
			}

			if resp.StatusCode >= 400 {
				// Other 4xx/5xx — log and pass through, not our problem to retry
				errBody, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				log.Printf("⚠️  Upstream %d for %s %s", resp.StatusCode, r.Method, target)
				log.Printf("  upstream error body: %s", string(errBody))
				// Reconstruct body for piping to client
				resp.Body = io.NopCloser(bytes.NewReader(errBody))
			}

			// ── Success: pipe response to client ──────────────────────
			for k, vals := range resp.Header {
				for _, v := range vals {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(resp.StatusCode)

			// Use low-level copy for streaming (SSE / chunked).
			if f, ok := w.(http.Flusher); ok {
				done := make(chan struct{})
				pr, pw := io.Pipe()
				go func() {
					defer close(done)
					buf := make([]byte, 4096)
					for {
						n, rerr := resp.Body.Read(buf)
						if n > 0 {
							pw.Write(buf[:n])
							f.Flush()
						}
						if rerr != nil {
							break
						}
					}
					pw.Close()
				}()
				io.Copy(w, pr)
				<-done
			} else {
				io.Copy(w, resp.Body)
			}

			resp.Body.Close()
			log.Printf("✅ %s %s → %d  (key[%d], attempt %d)", r.Method, target, resp.StatusCode, idx, attempt+1)
			return
		}

		http.Error(w, "hydra-proxy: exhausted all retries (all keys rate-limited)", http.StatusServiceUnavailable)
	}
}

// ─────────────────────────────────────────────
// Health endpoint
// ─────────────────────────────────────────────

func healthHandler(pool *KeyPool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","keys":%d,"pool":"%s"}`, len(pool.keys), pool.Status())
	}
}

// ─────────────────────────────────────────────
// Main
// ─────────────────────────────────────────────

func main() {
	loadDotEnv(".env") // simple .env loader

	cfg, pool := loadConfig()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler(pool))
	mux.HandleFunc("/", makeHandler(pool, cfg))

	addr := ":" + cfg.Port
	log.Printf("🐍 Hydra-Proxy started on %s", addr)
	log.Printf("   Target  : %s", cfg.TargetBase)
	log.Printf("   Keys    : %d loaded", len(pool.keys))
	log.Printf("   Cooldown: %ds per key on 429", cfg.CooldownSec)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// ─────────────────────────────────────────────
// Minimal .env loader (zero deps)
// ─────────────────────────────────────────────

func loadDotEnv(filename string) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return // .env is optional
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		// Don't overwrite real env vars
		if os.Getenv(key) == "" {
			os.Setenv(key, val)
		}
	}
}