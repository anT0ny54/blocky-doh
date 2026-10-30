package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var strictRawURLEncoding = base64.RawURLEncoding.Strict()

var (
	errMissingDNSParameter = errors.New("missing dns parameter")
	errInvalidDNSParameter = errors.New("invalid dns parameter")
	errDNSMessageTooLarge  = errors.New("dns message too large")
	errInvalidContentType  = errors.New("invalid content type")
	errFailedReadDNSBody   = errors.New("failed to read dns message")
	errNoClientIdentity    = errors.New("no valid client identity from trusted proxy")
)

const (
	defaultPort          = "8080"
	defaultBackendAddr   = "127.0.0.1:8053"
	defaultBackendURL    = "http://" + defaultBackendAddr + "/dns-query"
	defaultRate          = 99
	defaultWindow        = 60 * time.Second
	defaultMaxClients    = 8192
	defaultMaxConcurrent = 8 // also the hard cap: MAX_CONCURRENT can only lower it
	maxRateLimit         = 256
	maxLimiterClients    = 32768
	maxDNSBody           = 8192
	// Header + minimum question + minimum resource-record size. This is the
	// largest uncompressed record count that can fit in the gateway's bounded
	// DNS message.
	maxDNSResourceRecords = (maxDNSBody - 12 - 5) / 11
	clientIdleTTL         = 2 * time.Minute
	maxDNSNameHops        = 255
	maxDNSNameLength      = 255
	maxHeaderBytes        = 16 << 10
	healthProbeTimeout    = 250 * time.Millisecond
	healthProbeCacheTTL   = 2 * time.Second
	evictInterval         = time.Second
	sweepInterval         = 60 * time.Second
	logInterval           = 10 * time.Second
	// defaultIPv6Prefix groups IPv6 clients into one rate-limit identity per
	// routed prefix so address rotation inside a single prefix cannot mint
	// additional windows. 0 disables grouping (exact per-address identity).
	defaultIPv6Prefix = 64
	maxIPv6Prefix     = 128
	// defaultHealthRate bounds the public /healthz endpoint with its own
	// per-client-identity budget, independent of the DoH RATE_LIMIT.
	defaultHealthRate = 60
	// corsPreflightMaxAge lets browsers reuse the fixed CORS policy without
	// repeatedly spending a request on an OPTIONS preflight.
	corsPreflightMaxAge = "600"
	// defaultTrustHeader carries the HMAC-signed client identity from the
	// trusted edge when TRUST_SECRET is configured.
	defaultTrustHeader = "X-Real-Client-IP"
)

type limitResult int

const (
	limitAllowed limitResult = iota
	limitRateLimited
	limitCapacity
)

type clientState struct {
	mu    sync.Mutex
	times []int64 // sorted ascending; times[0] is the oldest, times[len-1] the newest
}

// limiter.mu must always be acquired before clientState.mu. All current paths
// preserve that order; taking clientState.mu first can deadlock against
// checkWithClock or evictStaleLocked.
type limiter struct {
	mu         sync.Mutex
	clients    map[string]*clientState
	maxClients int
	rate       int
	window     time.Duration
	// idleTTL is how long a client's newest accepted request must be in the
	// past before its state may be evicted. It is never shorter than the
	// window: a client that still has live timestamps must keep its state, or
	// eviction would reset its budget.
	idleTTL   time.Duration
	lastEvict time.Time
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
		idleTTL:    max(clientIdleTTL, window),
	}
}

// check samples the request time after acquiring per-client state, then
// snapshots any retry delay before releasing that state lock. This keeps
// concurrent requests for one client in the same order as their window updates.
func (l *limiter) check(ip string) (limitResult, time.Duration) {
	return l.checkWithClock(ip, time.Now)
}

func (l *limiter) checkWithClock(ip string, nowFunc func() time.Time) (limitResult, time.Duration) {
	l.mu.Lock()
	if l.clients == nil {
		l.clients = make(map[string]*clientState)
	}
	state := l.clients[ip]
	if state == nil {
		if len(l.clients) >= l.maxClients {
			now := nowFunc()
			if l.lastEvict.IsZero() || now.Sub(l.lastEvict) >= evictInterval {
				l.lastEvict = now
				l.evictStaleLocked(now)
			}
		}
		if len(l.clients) >= l.maxClients {
			l.mu.Unlock()
			return limitCapacity, 0
		}
		state = &clientState{}
		l.clients[ip] = state
	}
	state.mu.Lock()
	l.mu.Unlock()
	now := nowFunc()
	cutoff := now.Add(-l.window).UnixNano()
	// Timestamps are kept sorted, so expire the old prefix with a binary
	// search instead of scanning every request in the client's window.
	firstLive := sort.Search(len(state.times), func(i int) bool {
		return state.times[i] > cutoff
	})
	if firstLive == len(state.times) {
		state.times = state.times[:0]
	} else if firstLive > 0 {
		// Compact in place to preserve the slice capacity for future arrivals.
		copy(state.times, state.times[firstLive:])
		state.times = state.times[:len(state.times)-firstLive]
	}
	if len(state.times) >= l.rate {
		retryAfter := retryAfterDuration(state.times[0], now, l.window)
		state.mu.Unlock()
		return limitRateLimited, retryAfter
	}
	ts := now.UnixNano()
	// The production clock is sampled while the client lock is held, so
	// ordinary arrivals append at the end. Keep the slice sorted as a fallback
	// for clock regressions and deterministic out-of-order test timestamps.
	if len(state.times) == 0 || ts >= state.times[len(state.times)-1] {
		state.times = append(state.times, ts)
	} else {
		index := sort.Search(len(state.times), func(i int) bool {
			return state.times[i] >= ts
		})
		state.times = append(state.times, 0)
		copy(state.times[index+1:], state.times[index:len(state.times)-1])
		state.times[index] = ts
	}
	state.mu.Unlock()
	return limitAllowed, 0
}

func (l *limiter) evictStaleLocked(now time.Time) {
	cutoff := now.Add(-l.idleTTL).UnixNano()
	for ip, state := range l.clients {
		state.mu.Lock()
		// Timestamps are sorted, so the last one is the newest. A client with
		// any live timestamp is newer than the cutoff because idleTTL >= window,
		// so eviction can never reset a live budget.
		stale := len(state.times) == 0 || state.times[len(state.times)-1] < cutoff
		state.mu.Unlock()
		if stale {
			delete(l.clients, ip)
		}
	}
}

func retryAfterDuration(oldest int64, now time.Time, window time.Duration) time.Duration {
	wait := time.Unix(0, oldest).Add(window).Sub(now)
	// A clock regression or deterministic test can put an older timestamp after
	// a newer observation. Never advertise a wait longer than the configured
	// window, and never less than one second.
	return max(time.Second, min(wait, max(window, time.Second)))
}

func retryAfterSeconds(d time.Duration) int {
	return max(1, int((d+time.Second-1)/time.Second))
}

// limiterKey maps a normalized client IP to its rate-limit identity. IPv4
// addresses are always individual. IPv6 addresses are masked to the configured
// prefix so a subscriber rotating addresses inside one routed prefix cannot
// obtain additional windows; a prefix of 0 keeps exact per-address identities.
func limiterKey(ip string, v6Prefix int) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	// A zone selects the local interface for routing; it is not part of the
	// remote client's address or its rate-limit identity.
	addr = addr.WithZone("").Unmap()
	if addr.Is4() || v6Prefix <= 0 {
		return addr.String()
	}
	if v6Prefix > maxIPv6Prefix {
		v6Prefix = maxIPv6Prefix
	}
	return netip.PrefixFrom(addr, v6Prefix).Masked().String()
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
	ticker := time.NewTicker(sweepInterval)
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

	// healthLimiter bounds /healthz with its own per-client-identity budget, kept
	// separate from the DoH budget so a public health endpoint cannot be
	// abused as an unthrottled request driver. Nil in hand-built test servers:
	// the health handler skips the budget when unset.
	healthLimiter *limiter
	// v6Prefix groups IPv6 clients into one rate-limit identity per routed
	// prefix; 0 keeps exact per-address identities.
	v6Prefix int
	// trustedProxies restricts proxy-header trust to these networks. Headers
	// from any other peer are ignored even when trustProxy is set, so
	// TRUST_PROXY cannot be abused on a directly reachable port. Nil in
	// hand-built test servers means no peer is trusted.
	trustedProxies []*net.IPNet
	// trustSecret, when non-empty, enables HMAC-SHA256 verification of the
	// client-identity header: only a value with a valid signature from a
	// trusted peer is used. trustHeader names that header and requireIdentity
	// rejects (instead of peer-bucketing) a non-loopback trusted peer that
	// presents no valid identity.
	trustSecret  string
	trustHeader  string
	requireIdent bool

	// Cached readiness probe so a public health endpoint cannot force a TCP
	// dial on every request.
	probeMu sync.Mutex
	probeAt time.Time
	probeOK bool
}

func parseEnvInt(name string, def, upper int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return def
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 || n > upper {
		return def
	}
	return n
}

// parseEnvIPv6Prefix accepts 0 (disable grouping) through 128; anything else
// falls back to the default.
func parseEnvIPv6Prefix(name string, def int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return def
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 0 || n > maxIPv6Prefix {
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

// parseCIDRList parses a comma-separated list of IPs and CIDRs. Bare IPs are
// treated as exact /32 (IPv4) or /128 (IPv6) matches; empty and invalid
// entries are skipped.
func parseCIDRList(value string) []*net.IPNet {
	var out []*net.IPNet
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if ip := net.ParseIP(part); ip != nil {
			bits := 128
			if ip.To4() != nil {
				bits = 32
			}
			out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		if _, network, err := net.ParseCIDR(part); err == nil {
			out = append(out, network)
		}
	}
	return out
}

// defaultTrustedProxies covers the non-loopback networks a platform load
// balancer typically connects from: link-local, IPv6 ULA and RFC 1918 private
// ranges. Loopback peers are trusted separately by clientIdentity, so they do
// not need duplicate entries here. Used only when TRUST_PROXY=true and
// TRUSTED_PROXIES is unset.
func defaultTrustedProxies() []*net.IPNet {
	return parseCIDRList("169.254.0.0/16, fe80::/10, " +
		"10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, fc00::/7")
}

// configuredTrustedProxies uses broad platform-network defaults only when the
// setting is absent. An explicitly supplied empty or invalid list fails
// closed, rather than unexpectedly widening trust to every private network.
func configuredTrustedProxies(trustProxy bool, value string, configured bool) []*net.IPNet {
	if !trustProxy {
		return nil
	}
	if !configured {
		return defaultTrustedProxies()
	}
	return parseCIDRList(value)
}

// peerTrusted reports whether ip belongs to one of the configured trusted
// proxy networks.
func (s *server) peerTrusted(ip net.IP) bool {
	if ip == nil {
		return false
	}
	for _, network := range s.trustedProxies {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// forwardedIdentity extracts the client identity from conventional proxy
// headers (CF-Connecting-IP, then the first valid X-Forwarded-For entry, then
// X-Real-IP), returning "" when none carries a valid address.
func forwardedIdentity(r *http.Request) string {
	parse := func(value string) string {
		ip := net.ParseIP(strings.Trim(strings.TrimSpace(value), "[]"))
		if ip == nil {
			return ""
		}
		return ip.String()
	}
	// Prefer the header that trusted edges use to carry the original client
	// address. X-Forwarded-For is then interpreted using its conventional
	// left-most client address rather than the proxy's right-most address.
	if ip := parse(r.Header.Get("CF-Connecting-IP")); ip != "" {
		return ip
	}
	for _, value := range r.Header.Values("X-Forwarded-For") {
		// strings.Cut walks the list without allocating a slice per request.
		for value != "" {
			var part string
			part, value, _ = strings.Cut(value, ",")
			if ip := parse(part); ip != "" {
				return ip
			}
		}
	}
	if ip := parse(r.Header.Get("X-Real-IP")); ip != "" {
		return ip
	}
	return ""
}

// peerIPString returns the bare TCP peer address of the request.
func peerIPString(r *http.Request) string {
	host := strings.TrimSpace(r.RemoteAddr)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return host
}

// normalizedPeerIP returns the normalized TCP peer address and its parsed IP
// (when the address is an IP literal). Proxy identity headers are never
// consulted here; clientIdentity decides when those headers may be trusted.
func normalizedPeerIP(r *http.Request) (string, net.IP) {
	host := peerIPString(r)
	addr, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return host, nil
	}
	addr = addr.WithZone("").Unmap()
	return addr.String(), net.IP(addr.AsSlice())
}

// clientIP returns the normalized TCP peer address of the request.
func clientIP(r *http.Request) string {
	peer, _ := normalizedPeerIP(r)
	return peer
}

// validIdentitySignature reports whether sig is the correct lowercase hex
// HMAC-SHA256 of value under the shared trust secret. The signature covers
// the exact identity header value, so a client cannot forge or tamper with
// it without the secret.
func validIdentitySignature(secret, value, sig string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(value))
	expected := hex.EncodeToString(mac.Sum(nil))
	return sig != "" && hmac.Equal([]byte(expected), []byte(sig))
}

// clientIdentity resolves the rate-limit identity for a request. Proxy
// identity headers are consulted only when the TCP peer is a configured
// trusted proxy; loopback peers are inherently trusted so the container
// health check keeps working without identity headers. When trustSecret is
// configured, only the signed identity header is accepted, so a client-
// supplied header value cannot forge an identity even if the edge forwards
// it un-sanitized. When requireIdentity is set, a non-loopback trusted peer
// that presents no valid identity is rejected instead of being attributed
// to the proxy itself.
func (s *server) clientIdentity(r *http.Request) (string, error) {
	if !s.trustProxy {
		return clientIP(r), nil
	}
	// Parse and normalize the TCP peer once: the result drives the
	// proxy-trust decision, loopback exemption, and safe fallback identity.
	peerAddress, peer := normalizedPeerIP(r)
	loopback := peer != nil && peer.IsLoopback()
	if !loopback && !s.peerTrusted(peer) {
		// Untrusted peer: never honor proxy headers.
		return peerAddress, nil
	}
	var ident string
	if s.trustSecret != "" {
		// Verify the exact header value before normalizing it as an IP. The
		// signature format is lowercase hex, matching the documented edge
		// contract; do not silently accept alternate casing or whitespace in it.
		value := r.Header.Get(s.trustHeader)
		if value != "" && validIdentitySignature(s.trustSecret, value, r.Header.Get(s.trustHeader+"-Sig")) {
			if ip := net.ParseIP(strings.Trim(strings.TrimSpace(value), "[]")); ip != nil {
				ident = ip.String()
			}
		}
	} else {
		ident = forwardedIdentity(r)
	}
	if ident != "" {
		return ident, nil
	}
	if s.requireIdent && !loopback {
		return "", errNoClientIdentity
	}
	return peerAddress, nil
}

func (s *server) backendReady() bool {
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	if time.Since(s.probeAt) < healthProbeCacheTTL {
		return s.probeOK
	}
	conn, err := net.DialTimeout("tcp", s.backendAddr, healthProbeTimeout)
	s.probeAt = time.Now()
	s.probeOK = err == nil
	if err == nil {
		_ = conn.Close()
	}
	return s.probeOK
}

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeGatewayError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	// The health endpoint is public, so it gets its own coarse per-client-identity
	// budget (independent of the DoH budget) on top of the cached probe. A nil
	// healthLimiter (hand-built test servers) disables the budget.
	if s.healthLimiter != nil {
		ip, identErr := s.clientIdentity(r)
		if identErr != nil {
			writeGatewayError(w, http.StatusServiceUnavailable, "client identity unavailable")
			return
		}
		// Apply the same IPv6 prefix grouping as DoH so address rotation
		// within one routed prefix cannot multiply the public health budget.
		decision, retryAfter := s.healthLimiter.check(limiterKey(ip, s.v6Prefix))
		switch decision {
		case limitAllowed:
		case limitRateLimited:
			w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(retryAfter)))
			writeGatewayError(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		case limitCapacity:
			writeGatewayError(w, http.StatusServiceUnavailable, "server busy")
			return
		}
	}
	if !s.backendReady() {
		writeGatewayError(w, http.StatusServiceUnavailable, "dns backend unavailable")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	// Explicit length keeps the 2-byte body off the chunked-encoding path.
	w.Header().Set("Content-Length", "2")
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
		// The bound above also caps the decoded message at maxDNSBody.
		params, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			return nil, errInvalidDNSParameter
		}
		dnsValues := params["dns"]
		if len(dnsValues) == 0 || dnsValues[0] == "" {
			return nil, errMissingDNSParameter
		}
		if len(dnsValues) != 1 {
			return nil, errInvalidDNSParameter
		}
		encoded := dnsValues[0]
		// The strict decoder rejects padding and every byte outside the base64url
		// alphabet except CR/LF, which it silently skips; a DoH GET value must be
		// a single unpadded token, so refuse those two explicitly.
		if strings.ContainsAny(encoded, "\r\n") {
			return nil, errInvalidDNSParameter
		}
		decoded, err := strictRawURLEncoding.DecodeString(encoded)
		if err != nil {
			return nil, errInvalidDNSParameter
		}
		return decoded, nil
	}
	if !validContentType(r.Header.Get("Content-Type")) {
		return nil, errInvalidContentType
	}
	if r.ContentLength > maxDNSBody {
		return nil, errDNSMessageTooLarge
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

// walkDNSName follows the (possibly compressed) name starting at offset and
// returns the offset of the first byte after it at its original position. When
// canonical is non-nil, an unambiguous, ASCII-case-folded wire name is appended
// so equality checks preserve label boundaries instead of joining labels with dots.
// Compression pointers must point strictly backwards (so always inside the
// message) and past the header, and both the number of pointer hops and the
// expanded wire length are bounded.
func walkDNSName(message []byte, offset int, canonical *[]byte) (int, error) {
	if offset < 0 || offset >= len(message) {
		return 0, errors.New("dns name offset out of bounds")
	}
	pos := offset
	next := -1
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
			if pointer < 12 || pointer >= pos {
				return 0, errors.New("invalid dns compression pointer")
			}
			if next < 0 {
				next = pos + 2
			}
			hops++
			if hops > maxDNSNameHops {
				return 0, errors.New("too many dns compression pointer hops")
			}
			pos = pointer
		case 0x00:
			pos++
			if length == 0 {
				nameLen++
				if nameLen > maxDNSNameLength {
					return 0, errors.New("dns name too long")
				}
				if canonical != nil {
					*canonical = append(*canonical, 0)
				}
				if next < 0 {
					next = pos
				}
				return next, nil
			}
			if pos+length > len(message) {
				return 0, errors.New("truncated dns label")
			}
			nameLen += 1 + length
			if nameLen > maxDNSNameLength {
				return 0, errors.New("dns name too long")
			}
			label := message[pos : pos+length]
			if canonical != nil {
				*canonical = append(*canonical, byte(length))
				labelStart := len(*canonical)
				*canonical = append(*canonical, label...)
				for i := labelStart; i < len(*canonical); i++ {
					if c := (*canonical)[i]; c >= 'A' && c <= 'Z' {
						(*canonical)[i] = c + ('a' - 'A')
					}
				}
			}
			pos += length
		default:
			return 0, errors.New("invalid dns label type")
		}
	}
}

// canonicalNameBufPool recycles the scratch buffers used to build canonical
// wire names, avoiding a fresh allocation per DNS question comparison.
var canonicalNameBufPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 0, maxDNSNameLength)
		return &buf
	},
}

func dnsQuestionEqual(query, response []byte) bool {
	qBuf := canonicalNameBufPool.Get().(*[]byte)
	rBuf := canonicalNameBufPool.Get().(*[]byte)
	qName := (*qBuf)[:0]
	rName := (*rBuf)[:0]
	defer func() {
		*qBuf, *rBuf = qName, rName
		canonicalNameBufPool.Put(qBuf)
		canonicalNameBufPool.Put(rBuf)
	}()
	qEnd, err := walkDNSName(query, 12, &qName)
	if err != nil {
		return false
	}
	rEnd, err := walkDNSName(response, 12, &rName)
	if err != nil {
		return false
	}
	if !bytes.Equal(qName, rName) || qEnd+4 > len(query) || rEnd+4 > len(response) {
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
	records := anCount + nsCount + arCount
	if records > maxDNSResourceRecords {
		return errors.New("dns resource record count exceeds limit")
	}
	// Exactly one question (checked above) needs at least a root label byte
	// plus 4 question bytes; every resource record needs at least a root
	// label byte plus 10 fixed bytes.
	if 5+records*11 > len(message)-12 {
		return errors.New("dns section counts exceed message size")
	}
	offset, err := walkDNSName(message, 12, nil)
	if err != nil {
		return err
	}
	if offset+4 > len(message) {
		return errors.New("truncated dns question")
	}
	offset += 4
	for i := 0; i < records; i++ {
		offset, err = walkDNSName(message, offset, nil)
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
	if offset != len(message) {
		return errors.New("trailing bytes after dns message")
	}
	return nil
}

func validContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && strings.EqualFold(mediaType, "application/dns-message")
}

func isTimeoutError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func (s *server) handleDoH(w http.ResponseWriter, r *http.Request) {
	// Keep browser clients able to inspect both successful DNS replies and
	// gateway errors. This public endpoint never enables credentialed CORS.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Expose-Headers", "Retry-After")
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "accept, content-type")
		w.Header().Set("Access-Control-Max-Age", corsPreflightMaxAge)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST, OPTIONS")
		writeGatewayError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	select {
	case s.concurrent <- struct{}{}:
	default:
		writeGatewayError(w, http.StatusServiceUnavailable, "server busy")
		return
	}
	// The slot covers parsing and upstream work only. It is released as soon
	// as the response is verified (and on every early return), so a client
	// that reads slowly cannot pin one of the few slots while its reply drains.
	slotHeld := true
	releaseSlot := func() {
		if slotHeld {
			slotHeld = false
			<-s.concurrent
		}
	}
	defer releaseSlot()
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
	// The rate-limit budget is charged only after body validation, so malformed
	// requests do not consume a client's window; the concurrency gate above
	// still bounds body parsing and upstream work. Proxy identity headers are
	// honored only when the TCP peer is a configured trusted proxy, so this
	// setting cannot be abused on a directly reachable port; a trusted peer
	// without a valid identity is rejected in strict mode instead of being
	// bucketed under the proxy.
	ip, identErr := s.clientIdentity(r)
	if identErr != nil {
		writeGatewayError(w, http.StatusServiceUnavailable, "client identity unavailable")
		return
	}
	// IPv6 clients share one rate window per configured prefix, so rotating
	// addresses inside a single routed prefix cannot mint additional windows.
	decision, retryAfter := s.limiter.check(limiterKey(ip, s.v6Prefix))
	switch decision {
	case limitAllowed:
	case limitRateLimited:
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(retryAfter)))
		writeGatewayError(w, http.StatusTooManyRequests, "rate limit exceeded")
		return
	default:
		if n, ok := s.capacityLog.allow(time.Now()); ok {
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
		// A client disconnect cancels the request context and needs no response.
		// An independent cancellation from the transport is still an upstream
		// failure and must not leave the handler's default status as 200.
		if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
			return
		}
		status := http.StatusBadGateway
		if isTimeoutError(err) {
			status = http.StatusGatewayTimeout
		}
		if n, ok := s.upstreamLog.allow(time.Now()); ok {
			log.Printf("upstream DNS request failed: %v%s", err, suppressedNote(n))
		}
		writeGatewayError(w, status, "upstream DNS service unavailable")
		return
	}
	defer resp.Body.Close()
	// Reject bad status, content type or declared size before reading the
	// body, so error responses are never buffered.
	if resp.StatusCode != http.StatusOK || !validContentType(resp.Header.Get("Content-Type")) || resp.ContentLength > maxDNSBody {
		writeGatewayError(w, http.StatusBadGateway, "invalid upstream DNS response")
		return
	}
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, maxDNSBody+1))
	if err != nil {
		// Body reads can time out after the backend has already sent response
		// headers. Classify those failures by the same deadline/timeout rules
		// as Client.Do instead of incorrectly reporting them as malformed data.
		if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
			return
		}
		status := http.StatusBadGateway
		if isTimeoutError(err) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		if n, ok := s.upstreamLog.allow(time.Now()); ok {
			log.Printf("upstream DNS response read failed: %v%s", err, suppressedNote(n))
		}
		writeGatewayError(w, status, "upstream DNS service unavailable")
		return
	}
	if len(responseBody) > maxDNSBody {
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
	// DNS responses must copy the query's opcode and recursion-desired bit.
	// Bit 7 is QR and is expected to differ; mask 0x79 selects opcode + RD.
	if query[2]&0x79 != responseBody[2]&0x79 {
		writeGatewayError(w, http.StatusBadGateway, "upstream DNS response opcode or RD mismatch")
		return
	}
	if !dnsQuestionEqual(query, responseBody) {
		writeGatewayError(w, http.StatusBadGateway, "upstream DNS response question mismatch")
		return
	}
	releaseSlot()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/dns-message")
	// Explicit length avoids chunked encoding for responses over Go's 2 KiB
	// auto-length threshold.
	w.Header().Set("Content-Length", strconv.Itoa(len(responseBody)))
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
	v6Prefix := parseEnvIPv6Prefix("IPV6_PREFIX", defaultIPv6Prefix)
	healthRate := parseEnvInt("HEALTH_RATE", defaultHealthRate, maxRateLimit)
	// Proxy identity headers are honored only from these networks. When
	// TRUST_PROXY=true and TRUSTED_PROXIES is unset, trust the ranges a
	// platform load balancer typically connects from; an explicitly supplied
	// list (including an empty or invalid one) never widens to those defaults.
	trustedProxyValue, trustedProxyValueSet := os.LookupEnv("TRUSTED_PROXIES")
	trustedProxies := configuredTrustedProxies(trustProxy, trustedProxyValue, trustedProxyValueSet)
	// Optional shared secret enabling HMAC-SHA256 verification of the edge-
	// supplied client identity header; empty keeps the conventional headers.
	trustSecret := strings.TrimSpace(os.Getenv("TRUST_SECRET"))
	trustHeader := strings.TrimSpace(os.Getenv("TRUST_HEADER"))
	if trustHeader == "" {
		trustHeader = defaultTrustHeader
	}
	// Strict identity: reject (rather than peer-bucket) requests from a
	// non-loopback trusted proxy that carries no valid identity.
	requireIdent := parseEnvBool("TRUST_REQUIRE_IDENTITY", true)
	lim := newLimiter(rate, defaultWindow, maxClients)
	healthLim := newLimiter(healthRate, defaultWindow, maxClients)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go lim.cleanup(ctx)
	go healthLim.cleanup(ctx)
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
		backendURL:     defaultBackendURL,
		backendAddr:    defaultBackendAddr,
		limiter:        lim,
		concurrent:     make(chan struct{}, maxConcurrent),
		trustProxy:     trustProxy,
		healthLimiter:  healthLim,
		v6Prefix:       v6Prefix,
		trustedProxies: trustedProxies,
		trustSecret:    trustSecret,
		trustHeader:    trustHeader,
		requireIdent:   requireIdent,
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
	log.Printf("DoH gateway listening on %s; rate=%d requests/%s sliding window; maxClients=%d; maxConcurrent=%d; trustProxy=%t; ipv6Prefix=/%d; healthRate=%d; trustedProxies=%d; trustSecret=%t; requireIdentity=%t", port, rate, defaultWindow, maxClients, maxConcurrent, trustProxy, v6Prefix, healthRate, len(trustedProxies), trustSecret != "", requireIdent)
	serveErr := make(chan error, 1)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		serveErr <- srv.ListenAndServe()
	}()
	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
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
