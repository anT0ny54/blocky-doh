package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	errMissingDNSParameter = errors.New("missing dns parameter")
	errInvalidDNSParameter = errors.New("invalid dns parameter")
	errDNSMessageTooLarge  = errors.New("dns message too large")
	errInvalidContentType  = errors.New("invalid content type")
	errFailedReadDNSBody   = errors.New("failed to read dns message")
	errUnsupportedMethod   = errors.New("unsupported method")
)

const (
	defaultPort           = "8080"
	defaultBackendURL     = "http://127.0.0.1:8053/dns-query"
	defaultBackendAddr    = "127.0.0.1:8053"
	defaultRate           = 99
	defaultWindow         = 60 * time.Second
	defaultMaxClients     = 131072
	defaultMaxConcurrent  = 256
	maxDNSBody            = 65535
	maxDNSResourceRecords = 4096
	clientIdleTTL         = 2 * time.Minute
	maxDNSNameHops        = 255
	maxHeaderBytes        = 16 << 10
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
	return &limiter{
		maxClients: maxClients,
		interval:   interval,
		burstTol:   interval * time.Duration(rate-1),
	}
}

func (l *limiter) allow(ip string, now time.Time) bool {
	l.mu.Lock()
	if l.clients == nil {
		l.clients = make(map[string]*clientState)
	}
	state := l.clients[ip]
	if state == nil {
		if len(l.clients) >= l.maxClients {
			l.evictSomeLocked(now)
		}
		if len(l.clients) >= l.maxClients {
			// Never evict a live client state just to admit a new IP.
			// Preserving the existing state keeps the per-client limit intact
			// and avoids splitting one client's rate history across two states.
			l.mu.Unlock()
			return false
		}
		state = &clientState{}
		l.clients[ip] = state
	}

	// Keep the map lock while acquiring the state lock. Cleanup and eviction
	// use the same lock order, so a state cannot be deleted between lookup and use.
	state.mu.Lock()
	l.mu.Unlock()

	state.seen = now
	threshold := state.tat.Add(-l.burstTol)
	if state.tat.IsZero() || !now.Before(threshold) {
		base := state.tat
		if now.After(base) {
			base = now
		}
		state.tat = base.Add(l.interval)
		state.mu.Unlock()
		return true
	}
	state.mu.Unlock()
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
	client      *http.Client
	backendURL  string
	backendAddr string
	limiter     *limiter
	concurrent  chan struct{}
	trustProxy  bool
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

func parseEnvBool(name string, def bool) bool {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return def
	}
	switch {
	case strings.EqualFold(value, "true"):
		return true
	case strings.EqualFold(value, "false"):
		return false
	default:
		return def
	}
}

func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
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
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeGatewayError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	conn, err := net.DialTimeout("tcp", s.backendAddr, 250*time.Millisecond)
	if err != nil {
		writeGatewayError(w, http.StatusServiceUnavailable, "dns backend unavailable")
		return
	}
	_ = conn.Close()

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write([]byte("ok"))
	}
}

func writeGatewayError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, msg, code)
}

func decodeDoHQuery(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	if r.Method == http.MethodGet {
		encoded := r.URL.Query().Get("dns")
		if encoded == "" {
			return nil, errMissingDNSParameter
		}
		decoded, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil {
			return nil, errInvalidDNSParameter
		}
		if len(decoded) > maxDNSBody {
			return nil, errDNSMessageTooLarge
		}
		return decoded, nil
	}

	if r.Method != http.MethodPost {
		return nil, errUnsupportedMethod
	}

	if !validContentType(r.Header.Get("Content-Type")) {
		return nil, errInvalidContentType
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxDNSBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, errDNSMessageTooLarge
		}
		return nil, errFailedReadDNSBody
	}
	return body, nil
}

func skipDNSName(message []byte, offset int) (int, error) {
	if offset < 0 || offset >= len(message) {
		return 0, errors.New("dns name offset out of bounds")
	}

	pos := offset
	returnOffset := offset
	jumped := false
	hops := 0
	nameLen := 0
	for {
		if pos >= len(message) {
			return 0, errors.New("truncated dns name")
		}
		length := int(message[pos])
		switch length & 0xc0 {
		case 0xc0:
			if pos+1 >= len(message) {
				return 0, errors.New("truncated dns compression pointer")
			}
			pointer := ((length & 0x3f) << 8) | int(message[pos+1])
			if pointer < 12 || pointer >= len(message) || pointer >= pos {
				return 0, errors.New("invalid dns compression pointer")
			}
			if !jumped {
				returnOffset = pos + 2
				jumped = true
			}
			hops++
			if hops > maxDNSNameHops {
				return 0, errors.New("dns compression pointer loop")
			}
			pos = pointer
		case 0x00:
			pos++
			if length == 0 {
				nameLen++
				if nameLen > 255 {
					return 0, errors.New("dns name too long")
				}
				if jumped {
					return returnOffset, nil
				}
				return pos, nil
			}
			if length > 63 {
				return 0, errors.New("dns label too long")
			}
			if pos+length > len(message) {
				return 0, errors.New("truncated dns label")
			}
			nameLen += 1 + length
			if nameLen > 255 {
				return 0, errors.New("dns name too long")
			}
			pos += length
		default:
			return 0, errors.New("invalid dns label type")
		}
	}
}

func validateDNSMessage(message []byte, response bool) error {
	if len(message) < 12 {
		return errors.New("dns message too short")
	}

	flags := uint16(message[2])<<8 | uint16(message[3])
	qr := flags&0x8000 != 0
	if qr != response {
		return errors.New("unexpected dns QR flag")
	}
	if !response && uint16(message[4])<<8|uint16(message[5]) == 0 {
		return errors.New("dns query has no question")
	}

	qdCount := int(uint16(message[4])<<8 | uint16(message[5]))
	anCount := int(uint16(message[6])<<8 | uint16(message[7]))
	nsCount := int(uint16(message[8])<<8 | uint16(message[9]))
	arCount := int(uint16(message[10])<<8 | uint16(message[11]))
	if qdCount != 1 {
		return errors.New("dns message must contain exactly one question")
	}
	if anCount+nsCount+arCount > maxDNSResourceRecords {
		return errors.New("dns resource record count exceeds limit")
	}
	// A question needs at least a root label plus QTYPE/QCLASS (5 bytes);
	// an RR needs at least a root owner plus its fixed 10-byte header.
	// Reject impossible counts early so hostile packets cannot force large
	// parser loops before the normal bounds checks fail.
	minBytes := qdCount*5 + (anCount+nsCount+arCount)*11
	if minBytes > len(message)-12 {
		return errors.New("dns section counts exceed message size")
	}
	offset := 12

	for i := 0; i < qdCount; i++ {
		var err error
		offset, err = skipDNSName(message, offset)
		if err != nil {
			return err
		}
		if offset+4 > len(message) {
			return errors.New("truncated dns question")
		}
		offset += 4
	}

	for _, count := range []int{anCount, nsCount, arCount} {
		for i := 0; i < count; i++ {
			var err error
			offset, err = skipDNSName(message, offset)
			if err != nil {
				return err
			}
			if offset+10 > len(message) {
				return errors.New("truncated dns resource record")
			}
			rdLength := int(uint16(message[offset+8])<<8 | uint16(message[offset+9]))
			offset += 10
			if offset+rdLength > len(message) {
				return errors.New("truncated dns rdata")
			}
			offset += rdLength
		}
	}

	if offset != len(message) {
		return errors.New("trailing bytes after dns message")
	}
	return nil
}

func validContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && strings.EqualFold(mediaType, "application/dns-message")
}

func (s *server) handleDoH(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/dns-query" {
		writeGatewayError(w, http.StatusNotFound, "not found")
		return
	}

	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "accept, content-type")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST, OPTIONS")
		writeGatewayError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	if !s.limiter.allow(clientIP(r, s.trustProxy), time.Now()) {
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

	query, err := decodeDoHQuery(w, r)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, errInvalidContentType) {
			writeGatewayError(w, http.StatusUnsupportedMediaType, "content-type must be application/dns-message")
			return
		}
		if errors.Is(err, errDNSMessageTooLarge) {
			writeGatewayError(w, http.StatusRequestEntityTooLarge, "dns message too large")
			return
		}
		writeGatewayError(w, code, err.Error())
		return
	}

	if err := validateDNSMessage(query, false); err != nil {
		writeGatewayError(w, http.StatusBadRequest, "invalid dns message")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	backendReq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.backendURL, bytes.NewReader(query))
	if err != nil {
		writeGatewayError(w, http.StatusInternalServerError, "request creation failed")
		return
	}
	backendReq.Header.Set("Content-Type", "application/dns-message")
	backendReq.Header.Set("Accept", "application/dns-message")
	backendReq.Header.Set("User-Agent", "hagezi-doh/1.0")
	backendReq.ContentLength = int64(len(query))

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
	if resp.ContentLength > maxDNSBody {
		writeGatewayError(w, http.StatusBadGateway, "invalid upstream DNS response")
		return
	}

	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, maxDNSBody+1))
	if err != nil {
		writeGatewayError(w, http.StatusBadGateway, "invalid upstream DNS response")
		return
	}
	if len(responseBody) > maxDNSBody || resp.StatusCode != http.StatusOK || !validContentType(resp.Header.Get("Content-Type")) {
		writeGatewayError(w, http.StatusBadGateway, "invalid upstream DNS response")
		return
	}
	if err := validateDNSMessage(responseBody, true); err != nil {
		writeGatewayError(w, http.StatusBadGateway, "invalid upstream DNS response")
		return
	}
	if len(responseBody) < 2 || len(query) < 2 || !bytes.Equal(responseBody[:2], query[:2]) {
		writeGatewayError(w, http.StatusBadGateway, "upstream DNS transaction ID mismatch")
		return
	}

	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/dns-message")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(responseBody)
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
	trustProxy := parseEnvBool("TRUST_PROXY", false)
	lim := newLimiter(rate, defaultWindow, maxClients)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go lim.cleanup(ctx)

	transport := &http.Transport{
		Proxy:                  nil,
		MaxIdleConns:           64,
		MaxIdleConnsPerHost:    64,
		MaxConnsPerHost:        maxConcurrent,
		IdleConnTimeout:        60 * time.Second,
		DisableCompression:     true,
		ResponseHeaderTimeout:  2500 * time.Millisecond,
		MaxResponseHeaderBytes: maxHeaderBytes,
		TLSHandshakeTimeout:    1500 * time.Millisecond,
	}

	s := &server{
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		backendURL:  defaultBackendURL,
		backendAddr: defaultBackendAddr,
		limiter:     lim,
		concurrent:  make(chan struct{}, maxConcurrent),
		trustProxy:  trustProxy,
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
		MaxHeaderBytes:    maxHeaderBytes,
	}

	log.Printf("DoH gateway listening on %s; rate=%d requests/%s; maxClients=%d; maxConcurrent=%d; trustProxy=%t", port, rate, defaultWindow, maxClients, maxConcurrent, trustProxy)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(fmt.Errorf("listen: %w", err))
	}
}
