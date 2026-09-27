package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultPort          = "8080"
	backendURL           = "http://127.0.0.1:8053/dns-query"
	defaultRate          = 99
	defaultWindow        = 60 * time.Second
	defaultMaxClients    = 131072
	defaultMaxConcurrent = 256
	maxDNSBody           = 65535
	clientIdleTTL        = 2 * time.Minute
)

type clientState struct {
	mu   sync.Mutex
	tat  time.Time
	seen time.Time
}

type limiter struct {
	mu         sync.Mutex
	clients    map[string]*clientState
	maxClients int
	interval   time.Duration // spacing between allowed requests
	burstTol   time.Duration // tolerance allowing the initial burst
}

func newLimiter(rate int, window time.Duration, maxClients int) *limiter {
	if rate < 1 {
		rate = defaultRate
	}
	if window <= 0 {
		window = defaultWindow
	}
	if maxClients < 1024 {
		maxClients = defaultMaxClients
	}
	interval := window / time.Duration(rate)
	// GCRA-style limiter: allows exactly 'rate' requests immediately from an idle client,
	// then spaces further requests by window/rate. This avoids the double-burst edge case
	// of a naive fixed-window counter.
	return &limiter{
		clients:    make(map[string]*clientState, 8192),
		maxClients: maxClients,
		interval:   interval,
		burstTol:   interval * time.Duration(rate-1),
	}
}

func (l *limiter) allow(ip string, now time.Time) bool {
	l.mu.Lock()
	state := l.clients[ip]
	if state == nil {
		if len(l.clients) >= l.maxClients {
			l.evictSomeLocked(now)
		}
		state = &clientState{seen: now}
		l.clients[ip] = state
	}
	l.mu.Unlock()

	state.mu.Lock()
	defer state.mu.Unlock()

	// Keep last-seen time under the same per-client lock as the timestamp state.
	state.seen = now
	threshold := state.tat.Add(-l.burstTol)
	if state.tat.IsZero() || !now.Before(threshold) {
		base := state.tat
		if now.After(base) {
			base = now
		}
		state.tat = base.Add(l.interval)
		return true
	}
	return false
}

func (l *limiter) evictSomeLocked(now time.Time) {
	cutoff := now.Add(-clientIdleTTL)
	removed := 0
	for ip, state := range l.clients {
		state.mu.Lock()
		stale := state.seen.Before(cutoff)
		state.mu.Unlock()
		if stale {
			delete(l.clients, ip)
			removed++
			if removed >= 2048 {
				return
			}
		}
	}
	// If every entry is active, remove a small arbitrary slice rather than allowing
	// unbounded memory growth under a distributed source-IP flood.
	if len(l.clients) >= l.maxClients {
		for ip := range l.clients {
			delete(l.clients, ip)
			removed++
			if removed >= 1024 {
				return
			}
		}
	}
}

func (l *limiter) cleanup(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			now := time.Now()
			cutoff := now.Add(-clientIdleTTL)
			l.mu.Lock()
			for ip, state := range l.clients {
				state.mu.Lock()
				stale := state.seen.Before(cutoff)
				state.mu.Unlock()
				if stale {
					delete(l.clients, ip)
				}
			}
			l.mu.Unlock()
		case <-ctx.Done():
			return
		}
	}
}

type server struct {
	client     *http.Client
	backend    *http.Request
	limiter    *limiter
	concurrent chan struct{}
	requests   atomic.Uint64
	rejected   atomic.Uint64
}

func parseEnvInt(name string, def int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return def
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 {
		return def
	}
	return n
}

func clientIP(r *http.Request) string {
	// SnapDeploy places the app behind a managed load balancer. For local/direct runs,
	// use RemoteAddr unless TRUST_PROXY=true is explicitly enabled.
	if strings.EqualFold(os.Getenv("TRUST_PROXY"), "true") {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			for i := len(parts) - 1; i >= 0; i-- {
				candidate := strings.TrimSpace(parts[i])
				if net.ParseIP(candidate) != nil {
					return candidate
				}
			}
		}
		if rip := strings.TrimSpace(r.Header.Get("X-Real-IP")); net.ParseIP(rip) != nil {
			return rip
		}
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err == nil && net.ParseIP(host) != nil {
		return host
	}
	return strings.TrimSpace(r.RemoteAddr)
}

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func writeGatewayError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, msg, code)
}

func (s *server) handleDoH(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/dns-query" {
		writeGatewayError(w, http.StatusNotFound, "not found")
		return
	}

	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "content-type")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST, OPTIONS")
		writeGatewayError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	if !s.limiter.allow(clientIP(r), time.Now()) {
		s.rejected.Add(1)
		w.Header().Set("Retry-After", "1")
		writeGatewayError(w, http.StatusTooManyRequests, "rate limit exceeded")
		return
	}

	select {
	case s.concurrent <- struct{}{}:
		defer func() { <-s.concurrent }()
	default:
		writeGatewayError(w, http.StatusServiceUnavailable, "server busy")
		return
	}

	if r.Method == http.MethodPost {
		ct := strings.ToLower(strings.TrimSpace(strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0]))
		if ct != "application/dns-message" {
			writeGatewayError(w, http.StatusUnsupportedMediaType, "content-type must be application/dns-message")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxDNSBody)
	}
	if r.Method == http.MethodGet && r.URL.Query().Get("dns") == "" {
		writeGatewayError(w, http.StatusBadRequest, "missing dns parameter")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	var body io.Reader
	if r.Body != nil {
		body = r.Body
	}
	backendReq, err := http.NewRequestWithContext(ctx, r.Method, backendURL, body)
	if err != nil {
		writeGatewayError(w, http.StatusInternalServerError, "request creation failed")
		return
	}
	backendReq.URL.RawQuery = r.URL.RawQuery
	backendReq.Header = make(http.Header, 4)
	if r.Method == http.MethodPost {
		backendReq.Header.Set("Content-Type", "application/dns-message")
	}
	backendReq.Header.Set("Accept", "application/dns-message")
	backendReq.Header.Set("User-Agent", "hagezi-doh/1.0")

	resp, err := s.client.Do(backendReq)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		writeGatewayError(w, status, "upstream DNS service unavailable")
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-store")
	for key, values := range resp.Header {
		if key == "Content-Length" || key == "Transfer-Encoding" || key == "Connection" {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, maxDNSBody+1))
	s.requests.Add(1)
}

func main() {
	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = defaultPort
	}
	if !strings.HasPrefix(port, ":") {
		port = ":" + port
	}

	rate := parseEnvInt("RATE_LIMIT", defaultRate)
	maxClients := parseEnvInt("MAX_CLIENTS", defaultMaxClients)
	maxConcurrent := parseEnvInt("MAX_CONCURRENT", defaultMaxConcurrent)
	lim := newLimiter(rate, defaultWindow, maxClients)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go lim.cleanup(ctx)

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   256,
		MaxConnsPerHost:       maxConcurrent,
		IdleConnTimeout:       60 * time.Second,
		DisableCompression:    true,
		ResponseHeaderTimeout: 2500 * time.Millisecond,
		TLSHandshakeTimeout:   1500 * time.Millisecond,
		ExpectContinueTimeout: 250 * time.Millisecond,
	}

	s := &server{
		client:     &http.Client{Transport: transport},
		limiter:    lim,
		concurrent: make(chan struct{}, maxConcurrent),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.health)
	mux.HandleFunc("/dns-query", s.handleDoH)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeGatewayError(w, http.StatusNotFound, "not found")
	})

	srv := &http.Server{
		Addr:              port,
		Handler:           mux,
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	log.Printf("DoH gateway listening on %s; rate=%d requests/%s; maxClients=%d; maxConcurrent=%d", port, rate, defaultWindow, maxClients, maxConcurrent)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(fmt.Errorf("listen: %w", err))
	}
}
