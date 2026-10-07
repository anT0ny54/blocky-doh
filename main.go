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
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)
var (
	errMissingDNSParameter = errors.New("missing dns parameter")
	errInvalidDNSParameter = errors.New("invalid dns parameter")
	errDNSMessageTooLarge  = errors.New("dns message too large")
	errInvalidContentType  = errors.New("invalid content type")
	errFailedReadDNSBody   = errors.New("failed to read dns message")
)
const (
	defaultPort           = "8080"
	defaultBackendURL     = "http://127.0.0.1:8053/dns-query"
	defaultBackendAddr    = "127.0.0.1:8053"
	defaultRate           = 99
	defaultWindow         = 60 * time.Second
	defaultMaxClients     = 256
	defaultMaxConcurrent  = 16
	maxRateLimit          = 256
	maxLimiterClients     = 1024
	maxDNSBody            = 65535
	maxDNSResourceRecords = 4096
	clientIdleTTL         = 2 * time.Minute
	maxDNSNameHops        = 255
	maxDNSNameLength      = 255
	maxHeaderBytes        = 16 << 10
	evictInterval         = time.Second
	logInterval           = 10 * time.Second
)
type limitResult int
const (
	limitAllowed limitResult = iota
	limitRateLimited
	limitCapacity
)
type clientState struct {
	mu    sync.Mutex
	times []time.Time
	seen  time.Time
}
type limiter struct {
	mu         sync.Mutex
	clients    map[string]*clientState
	maxClients int
	rate       int
	window     time.Duration
	lastEvict  time.Time
}
func newLimiter(rate int, window time.Duration, maxClients int) *limiter {
	if rate < 1 {
		rate = defaultRate
	}
	if window <= 0 {
		window = defaultWindow
	}
	if maxClients < 1 {
		maxClients = defaultMaxClients
	}
	return &limiter{
		maxClients: maxClients,
		rate:       rate,
		window:     window,
	}
}
func (l *limiter) check(ip string, now time.Time) limitResult {
	cutoff := now.Add(-l.window)
	l.mu.Lock()
	if l.clients == nil {
		l.clients = make(map[string]*clientState)
	}
	state := l.clients[ip]
	if state == nil {
		if len(l.clients) >= l.maxClients {
			if l.lastEvict.IsZero() || now.Sub(l.lastEvict) >= evictInterval {
				l.lastEvict = now
				l.evictStaleLocked(now)
			}
		}
		if len(l.clients) >= l.maxClients {
			l.mu.Unlock()
			return limitCapacity
		}
		state = &clientState{}
		l.clients[ip] = state
	}
	state.mu.Lock()
	l.mu.Unlock()
	kept := state.times[:0]
	for _, t := range state.times {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	state.times = kept
	// Refresh liveness for every request, including rejected ones, so a
	// persistently rate-limited client is not evicted as "idle" while it
	// is still actively (if unsuccessfully) reaching the gateway.
	state.seen = now
	if len(state.times) >= l.rate {
		state.mu.Unlock()
		return limitRateLimited
	}
	state.times = append(state.times, now)
	state.mu.Unlock()
	return limitAllowed
}
func (l *limiter) evictStaleLocked(now time.Time) {
	cutoff := now.Add(-clientIdleTTL)
	for ip, state := range l.clients {
		state.mu.Lock()
		stale := state.seen.Before(cutoff)
		state.mu.Unlock()
		if stale {
			delete(l.clients, ip)
		}
	}
}
func (l *limiter) retryAfter(ip string, now time.Time) time.Duration {
	l.mu.Lock()
	state := l.clients[ip]
	if state == nil {
		l.mu.Unlock()
		return time.Second
	}
	state.mu.Lock()
	l.mu.Unlock()
	defer state.mu.Unlock()
	if len(state.times) == 0 {
		return time.Second
	}
	if wait := state.times[0].Add(l.window).Sub(now); wait > time.Second {
		return wait
	}
	return time.Second
}
func retryAfterSeconds(d time.Duration) int {
	secs := int((d + time.Second - 1) / time.Second)
	if secs < 1 {
		return 1
	}
	return secs
}
type logGate struct {
	mu         sync.Mutex
	last       time.Time
	suppressed int64
}
func (g *logGate) allow(now time.Time) (int64, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.last.IsZero() && now.Sub(g.last) < logInterval {
		g.suppressed++
		return 0, false
	}
	g.last = now
	n := g.suppressed
	g.suppressed = 0
	return n, true
}
func suppressedNote(n int64) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d similar messages suppressed)", n)
}
func (l *limiter) cleanup(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			l.mu.Lock()
			l.evictStaleLocked(time.Now())
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
	capacityLog logGate
	upstreamLog logGate
}
func parseEnvInt(name string, def, max int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return def
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 || n > max {
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
		// Join every header line: a proxy may append its own line instead of
		// extending the first one, and Header.Get would then return only the
		// client-controlled line.
		if xff := strings.Join(r.Header.Values("X-Forwarded-For"), ","); xff != "" {
			parts := strings.Split(xff, ",")
			for i := len(parts) - 1; i >= 0; i-- {
				if ip := net.ParseIP(strings.TrimSpace(parts[i])); ip != nil {
					return ip.String()
				}
			}
		}
		if rip := strings.TrimSpace(r.Header.Get("X-Real-IP")); rip != "" {
			if ip := net.ParseIP(rip); ip != nil {
				return ip.String()
			}
		}
	}
	host := strings.TrimSpace(r.RemoteAddr)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return ip.String()
	}
	return host
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
		// Reject oversized query strings before parsing them. The dns=
		// parameter alone can never legitimately exceed the base64url
		// encoding of the maximum DNS message, so a longer raw query is
		// guaranteed to carry an oversized dns= value (or junk padding
		// intended to make the gateway parse megabytes of query string).
		if len(r.URL.RawQuery) > len("dns=")+base64.RawURLEncoding.EncodedLen(maxDNSBody) {
			return nil, errDNSMessageTooLarge
		}
		// The raw-query bound above already caps the dns= value at the
		// encoded size of maxDNSBody, so the decoded message cannot exceed
		// maxDNSBody either.
		encoded := r.URL.Query().Get("dns")
		if encoded == "" {
			return nil, errMissingDNSParameter
		}
		decoded, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil {
			return nil, errInvalidDNSParameter
		}
		return decoded, nil
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
				if nameLen > maxDNSNameLength {
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
			if nameLen > maxDNSNameLength {
				return 0, errors.New("dns name too long")
			}
			pos += length
		default:
			return 0, errors.New("invalid dns label type")
		}
	}
}
func expandDNSName(message []byte, offset int) (string, int, error) {
	var b strings.Builder
	pos := offset
	end := -1
	jumped := false
	hops := 0
	for {
		if pos >= len(message) {
			return "", 0, errors.New("truncated dns name")
		}
		length := int(message[pos])
		switch length & 0xc0 {
		case 0xc0:
			if pos+1 >= len(message) {
				return "", 0, errors.New("truncated dns compression pointer")
			}
			pointer := ((length & 0x3f) << 8) | int(message[pos+1])
			if pointer < 12 || pointer >= len(message) || pointer >= pos {
				return "", 0, errors.New("invalid dns compression pointer")
			}
			if !jumped {
				end = pos + 2
				jumped = true
			}
			hops++
			if hops > maxDNSNameHops {
				return "", 0, errors.New("dns compression pointer loop")
			}
			pos = pointer
		case 0x00:
			pos++
			if length == 0 {
				if !jumped {
					end = pos
				}
				return b.String(), end, nil
			}
			if length > 63 {
				return "", 0, errors.New("dns label too long")
			}
			if pos+length > len(message) {
				return "", 0, errors.New("truncated dns label")
			}
			if b.Len()+length > maxDNSNameLength {
				return "", 0, errors.New("dns name too long")
			}
			if b.Len() > 0 {
				b.WriteByte('.')
			}
			b.Write(message[pos : pos+length])
			pos += length
		default:
			return "", 0, errors.New("invalid dns label type")
		}
	}
}
func dnsNameEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if x >= 'A' && x <= 'Z' {
			x += 'a' - 'A'
		}
		if y >= 'A' && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}
func dnsQuestionEqual(query, response []byte) bool {
	qName, qEnd, err := expandDNSName(query, 12)
	if err != nil {
		return false
	}
	rName, rEnd, err := expandDNSName(response, 12)
	if err != nil {
		return false
	}
	if !dnsNameEqual(qName, rName) || qEnd+4 > len(query) || rEnd+4 > len(response) {
		return false
	}
	return bytes.Equal(query[qEnd:qEnd+4], response[rEnd:rEnd+4])
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
	query, err := decodeDoHQuery(w, r)
	if err != nil {
		if errors.Is(err, errInvalidContentType) {
			writeGatewayError(w, http.StatusUnsupportedMediaType, "content-type must be application/dns-message")
			return
		}
		if errors.Is(err, errDNSMessageTooLarge) {
			writeGatewayError(w, http.StatusRequestEntityTooLarge, "dns message too large")
			return
		}
		writeGatewayError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateDNSMessage(query, false); err != nil {
		writeGatewayError(w, http.StatusBadRequest, "invalid dns message")
		return
	}
	select {
	case s.concurrent <- struct{}{}:
		defer func() { <-s.concurrent }()
	default:
		writeGatewayError(w, http.StatusServiceUnavailable, "server busy")
		return
	}
	// The rate-limit budget is charged only after the request has passed
	// body validation and the concurrency gate, so malformed or
	// over-capacity requests never consume a client's window.
	ip := clientIP(r, s.trustProxy)
	now := time.Now()
	switch s.limiter.check(ip, now) {
	case limitAllowed:
	case limitRateLimited:
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(s.limiter.retryAfter(ip, now))))
		writeGatewayError(w, http.StatusTooManyRequests, "rate limit exceeded")
		return
	default:
		if n, ok := s.capacityLog.allow(now); ok {
			log.Printf("client limiter at capacity (%d states); rejecting new clients%s", s.limiter.maxClients, suppressedNote(n))
		}
		writeGatewayError(w, http.StatusServiceUnavailable, "server busy")
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
	resp, err := s.client.Do(backendReq)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		status := http.StatusBadGateway
		var netErr net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
			status = http.StatusGatewayTimeout
		}
		if n, ok := s.upstreamLog.allow(time.Now()); ok {
			log.Printf("upstream DNS request failed: %v%s", err, suppressedNote(n))
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
	if !bytes.Equal(responseBody[:2], query[:2]) {
		writeGatewayError(w, http.StatusBadGateway, "upstream DNS transaction ID mismatch")
		return
	}
	if !dnsQuestionEqual(query, responseBody) {
		writeGatewayError(w, http.StatusBadGateway, "upstream DNS response question mismatch")
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/dns-message")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(responseBody)
}
func (s *server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.health)
	mux.HandleFunc("/dns-query", s.handleDoH)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeGatewayError(w, http.StatusNotFound, "not found")
	})
	return mux
}
func main() {
	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = defaultPort
	}
	if !strings.HasPrefix(port, ":") {
		port = ":" + port
	}
	rate := parseEnvInt("RATE_LIMIT", defaultRate, maxRateLimit)
	maxClients := parseEnvInt("MAX_CLIENTS", defaultMaxClients, maxLimiterClients)
	maxConcurrent := parseEnvInt("MAX_CONCURRENT", defaultMaxConcurrent, defaultMaxConcurrent)
	trustProxy := parseEnvBool("TRUST_PROXY", false)
	lim := newLimiter(rate, defaultWindow, maxClients)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go lim.cleanup(ctx)
	transport := &http.Transport{
		MaxIdleConns:           maxConcurrent,
		MaxIdleConnsPerHost:    maxConcurrent,
		MaxConnsPerHost:        maxConcurrent,
		IdleConnTimeout:        30 * time.Second,
		DisableCompression:     true,
		ResponseHeaderTimeout:  2500 * time.Millisecond,
		MaxResponseHeaderBytes: maxHeaderBytes,
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
	srv := &http.Server{
		Addr:              port,
		Handler:           s.routes(),
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    maxHeaderBytes,
	}
	log.Printf("DoH gateway listening on %s; rate=%d requests/%s sliding window; maxClients=%d; maxConcurrent=%d; trustProxy=%t", port, rate, defaultWindow, maxClients, maxConcurrent, trustProxy)
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.ListenAndServe()
	}()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(fmt.Errorf("listen: %w", err))
		}
	case sig := <-sigCh:
		log.Printf("received %s; draining in-flight requests", sig)
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("graceful shutdown did not complete cleanly: %v", err)
		}
	}
}