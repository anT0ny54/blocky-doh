package main
import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)
func testDNSQuery(id byte) []byte {
	return []byte{
		id, 0x34, 0x01, 0x00,
		0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x07, 'e', 'x', 'a', 'm', 'p', 'l', 'e',
		0x03, 'c', 'o', 'm', 0x00,
		0x00, 0x01, 0x00, 0x01,
	}
}
func testDNSResponse(query []byte) []byte {
	return []byte{
		query[0], query[1], 0x81, 0x80,
		0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00,
		0x07, 'e', 'x', 'a', 'm', 'p', 'l', 'e',
		0x03, 'c', 'o', 'm', 0x00,
		0x00, 0x01, 0x00, 0x01,
		0xc0, 0x0c, 0x00, 0x01, 0x00, 0x01,
		0x00, 0x00, 0x00, 0x3c, 0x00, 0x04, 1, 2, 3, 4,
	}
}
func testServer(t *testing.T, backend func(http.ResponseWriter, *http.Request)) (*server, func()) {
	t.Helper()
	backendSrv := httptest.NewServer(http.HandlerFunc(backend))
	cleanup := func() { backendSrv.Close() }
	return &server{
		client:      backendSrv.Client(),
		backendURL:  backendSrv.URL,
		backendAddr: "127.0.0.1:1",
		limiter:     newLimiter(defaultRate, defaultWindow, 1024),
		concurrent:  make(chan struct{}, defaultMaxConcurrent),
	}, cleanup
}
func postDoH(s *server, query []byte, remoteAddr string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "http://gateway.test/dns-query", bytes.NewReader(query))
	req.Header.Set("Content-Type", "application/dns-message")
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	rec := httptest.NewRecorder()
	s.handleDoH(rec, req)
	return rec
}
func TestLimiterAllows99ThenRejects100thImmediateRequest(t *testing.T) {
	l := newLimiter(99, 60*time.Second, 1024)
	now := time.Unix(1_000_000, 0)
	for i := 0; i < 99; i++ {
		if got := l.check("203.0.113.10", now); got != limitAllowed {
			t.Fatalf("request %d got %v; want first 99 requests allowed", i+1, got)
		}
	}
	if got := l.check("203.0.113.10", now); got != limitRateLimited {
		t.Fatalf("100th immediate request got %v; want rate limiting", got)
	}
}
func TestLimiterSeparatesClientIPs(t *testing.T) {
	l := newLimiter(99, 60*time.Second, 1024)
	now := time.Unix(1_000_000, 0)
	for i := 0; i < 99; i++ {
		_ = l.check("203.0.113.10", now)
	}
	if got := l.check("198.51.100.20", now); got != limitAllowed {
		t.Fatalf("second client IP got %v; want allowed", got)
	}
}
func TestLimiterConcurrentAccessIsBoundedPerClient(t *testing.T) {
	l := newLimiter(99, 60*time.Second, 1024)
	now := time.Unix(1_000_000, 0)
	const goroutines = 32
	const attempts = 8
	results := make(chan limitResult, goroutines*attempts)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < attempts; j++ {
				results <- l.check("203.0.113.10", now)
			}
		}()
	}
	wg.Wait()
	close(results)
	allowed := 0
	for res := range results {
		if res == limitAllowed {
			allowed++
		} else if res != limitRateLimited {
			t.Fatalf("unexpected limiter result %v; want allowed or rate limited", res)
		}
	}
	if allowed != 99 {
		t.Fatalf("concurrent limiter allowed %d requests; want exactly 99", allowed)
	}
}
func TestLimiterAllocatesClientMapLazily(t *testing.T) {
	l := newLimiter(defaultRate, defaultWindow, 1024)
	if l.clients != nil {
		t.Fatal("limiter client map was allocated eagerly")
	}
	now := time.Unix(1_000_000, 0)
	if got := l.check("203.0.113.10", now); got != limitAllowed {
		t.Fatalf("first request got %v; want allowed", got)
	}
	if l.clients == nil {
		t.Fatal("limiter client map was not allocated on first request")
	}
}
func TestLimiterSlidingWindowAdmitsRequestsAgainAfterWindowSlides(t *testing.T) {
	l := newLimiter(99, 60*time.Second, 1024)
	start := time.Unix(2_000_000, 0)
	for i := 0; i < 99; i++ {
		if got := l.check("203.0.113.10", start); got != limitAllowed {
			t.Fatalf("request %d got %v; want first 99 requests allowed", i+1, got)
		}
	}
	if got := l.check("203.0.113.10", start.Add(30*time.Second)); got != limitRateLimited {
		t.Fatalf("request inside the window got %v; want rate limited", got)
	}
	if got := l.check("203.0.113.10", start.Add(60*time.Second)); got != limitAllowed {
		t.Fatalf("request after the window slid got %v; want allowed", got)
	}
}
func TestLimiterRejectsOnlyExpiredRequestsOutsideWindow(t *testing.T) {
	l := newLimiter(2, 60*time.Second, 1024)
	start := time.Unix(3_000_000, 0)
	if got := l.check("203.0.113.10", start); got != limitAllowed {
		t.Fatalf("first request got %v; want allowed", got)
	}
	if got := l.check("203.0.113.10", start.Add(30*time.Second)); got != limitAllowed {
		t.Fatalf("second request got %v; want allowed", got)
	}
	if got := l.check("203.0.113.10", start.Add(60*time.Second)); got != limitAllowed {
		t.Fatalf("request after one timestamp expired got %v; want allowed", got)
	}
	if got := l.check("203.0.113.10", start.Add(60*time.Second)); got != limitRateLimited {
		t.Fatalf("request with both timestamps in window got %v; want rate limited", got)
	}
}
func TestLimiterRetryAfterReflectsOldestTimestamp(t *testing.T) {
	l := newLimiter(2, 60*time.Second, 1024)
	start := time.Unix(4_000_000, 0)
	l.check("203.0.113.10", start)
	l.check("203.0.113.10", start.Add(30*time.Second))
	now := start.Add(31 * time.Second)
	if got := l.check("203.0.113.10", now); got != limitRateLimited {
		t.Fatalf("third request got %v; want rate limited", got)
	}
	if got := l.retryAfter("203.0.113.10", now); got != 29*time.Second {
		t.Fatalf("retryAfter=%v; want 29s until the oldest request leaves the window", got)
	}
	if got := l.retryAfter("198.51.100.99", now); got != time.Second {
		t.Fatalf("retryAfter for unknown client=%v; want 1s", got)
	}
}
func TestRetryAfterSecondsRoundsUpAndClamps(t *testing.T) {
	cases := map[time.Duration]int{
		-time.Second:            1,
		0:                       1,
		500 * time.Millisecond:  1,
		time.Second:             1,
		1500 * time.Millisecond: 2,
		60 * time.Second:        60,
	}
	for d, want := range cases {
		if got := retryAfterSeconds(d); got != want {
			t.Fatalf("retryAfterSeconds(%v)=%d; want %d", d, got, want)
		}
	}
}
func TestLimiterThrottlesEvictionScans(t *testing.T) {
	l := newLimiter(1, time.Minute, 1)
	now := time.Unix(6_000_000, 0)
	stale := now.Add(-clientIdleTTL - time.Second)
	l.clients = map[string]*clientState{
		"198.51.100.1": {times: []time.Time{stale}, seen: stale},
	}
	l.lastEvict = now
	if got := l.check("198.51.100.2", now.Add(500*time.Millisecond)); got != limitCapacity {
		t.Fatalf("check inside the eviction interval got %v; want limitCapacity (scan throttled)", got)
	}
	if got := l.check("198.51.100.2", now.Add(evictInterval)); got != limitAllowed {
		t.Fatalf("check after the eviction interval got %v; want allowed after stale-state eviction", got)
	}
}
func TestLogGateSuppressesBurstsAndReportsCount(t *testing.T) {
	var g logGate
	t0 := time.Unix(5_000_000, 0)
	if n, ok := g.allow(t0); !ok || n != 0 {
		t.Fatalf("first message: ok=%v suppressed=%d; want allowed with 0 suppressed", ok, n)
	}
	if _, ok := g.allow(t0.Add(time.Second)); ok {
		t.Fatal("message inside the interval was allowed")
	}
	if _, ok := g.allow(t0.Add(2 * time.Second)); ok {
		t.Fatal("second message inside the interval was allowed")
	}
	if n, ok := g.allow(t0.Add(logInterval)); !ok || n != 2 {
		t.Fatalf("message after the interval: ok=%v suppressed=%d; want allowed with 2 suppressed", ok, n)
	}
}
func TestLimiterDoesNotEvictLiveStateAtCapacity(t *testing.T) {
	l := newLimiter(1, time.Minute, 2)
	now := time.Now()
	if got := l.check("198.51.100.1", now); got != limitAllowed {
		t.Fatalf("first client got %v; want allowed", got)
	}
	if got := l.check("198.51.100.2", now); got != limitAllowed {
		t.Fatalf("second client got %v; want allowed", got)
	}
	if got := l.check("198.51.100.3", now); got != limitCapacity {
		t.Fatalf("new client at capacity got %v; want limitCapacity", got)
	}
	if got := l.check("198.51.100.1", now); got != limitRateLimited {
		t.Fatalf("existing client got %v; want limitRateLimited (state must not be split or reset)", got)
	}
}
func TestLimiterEvictsStaleStateAtCapacity(t *testing.T) {
	l := newLimiter(1, time.Minute, 2)
	now := time.Now()
	stale := now.Add(-clientIdleTTL - time.Second)
	l.clients = map[string]*clientState{
		"198.51.100.1": {times: []time.Time{stale}, seen: stale},
		"198.51.100.2": {times: []time.Time{now}, seen: now},
	}
	if got := l.check("198.51.100.3", now); got != limitAllowed {
		t.Fatalf("new client got %v; want allowed after stale-state eviction", got)
	}
	if _, ok := l.clients["198.51.100.1"]; ok {
		t.Fatal("stale client state was not evicted")
	}
	if _, ok := l.clients["198.51.100.2"]; !ok {
		t.Fatal("live client state was incorrectly evicted")
	}
}
func TestValidateDNSMessage(t *testing.T) {
	query := testDNSQuery(0x12)
	if err := validateDNSMessage(query, false); err != nil {
		t.Fatalf("valid query rejected: %v", err)
	}
	response := testDNSResponse(query)
	if err := validateDNSMessage(response, true); err != nil {
		t.Fatalf("valid response rejected: %v", err)
	}
	cases := []struct {
		name string
		msg  []byte
		rr   bool
	}{
		{"too short", []byte{0, 1}, false},
		{"query with QR set", func() []byte { b := append([]byte(nil), query...); b[2] |= 0x80; return b }(), false},
		{"response with QR clear", func() []byte { b := append([]byte(nil), response...); b[2] &^= 0x80; return b }(), true},
		{"query with no question", func() []byte { b := append([]byte(nil), query...); b[4], b[5] = 0, 0; return b }(), false},
		{"trailing bytes", append(append([]byte(nil), query...), 0xff), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateDNSMessage(tc.msg, tc.rr); err == nil {
				t.Fatal("invalid DNS message was accepted")
			}
		})
	}
}
func TestValidateDNSMessageRejectsCompressionPointerIntoHeader(t *testing.T) {
	message := append([]byte{
		0x12, 0x34, 0x01, 0x00,
		0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}, 0xc0, 0x02, 0x00, 0x01, 0x00, 0x01)
	if err := validateDNSMessage(message, false); err == nil {
		t.Fatal("compression pointer into DNS header was accepted")
	}
}
func TestValidateDNSMessageRejectsImpossibleSectionCounts(t *testing.T) {
	message := testDNSQuery(0x13)
	message[6], message[7] = 0xff, 0xff
	if err := validateDNSMessage(message, false); err == nil {
		t.Fatal("impossible section counts were accepted")
	}
}
func TestValidateDNSMessageRequiresExactlyOneQuestion(t *testing.T) {
	query := testDNSQuery(0x14)
	question := append([]byte(nil), query[12:]...)
	query = append(query, question...)
	query[4], query[5] = 0, 2
	if err := validateDNSMessage(query, false); err == nil {
		t.Fatal("query with multiple questions was accepted")
	}
	response := testDNSResponse(testDNSQuery(0x15))
	response = append(response, question...)
	response[4], response[5] = 0, 2
	if err := validateDNSMessage(response, true); err == nil {
		t.Fatal("response with multiple questions was accepted")
	}
}
func testDNSResponseWithAnswerRecords(query []byte, count int) []byte {
	message := []byte{
		query[0], query[1], 0x81, 0x80,
		0x00, 0x01, byte(count >> 8), byte(count),
		0x00, 0x00, 0x00, 0x00,
	}
	message = append(message, query[12:]...)
	for i := 0; i < count; i++ {
		message = append(message,
			0x00,
			0x00, 0x01, 0x00, 0x01,
			0x00, 0x00, 0x00, 0x00,
			0x00, 0x00,
		)
	}
	return message
}
func TestValidateDNSMessageEnforcesResourceRecordCeiling(t *testing.T) {
	query := testDNSQuery(0x16)
	withinLimit := testDNSResponseWithAnswerRecords(query, maxDNSResourceRecords)
	if err := validateDNSMessage(withinLimit, true); err != nil {
		t.Fatalf("response with %d records rejected: %v", maxDNSResourceRecords, err)
	}
	overLimit := testDNSResponseWithAnswerRecords(query, maxDNSResourceRecords+1)
	if err := validateDNSMessage(overLimit, true); err == nil {
		t.Fatalf("response with %d records was accepted", maxDNSResourceRecords+1)
	}
}
func TestValidateDNSMessageRejectsExpandedCompressedNameOver255Bytes(t *testing.T) {
	longName := make([]byte, 0, 253)
	for _, labelLen := range []int{63, 63, 63, 59} {
		longName = append(longName, byte(labelLen))
		longName = append(longName, bytes.Repeat([]byte{'a'}, labelLen)...)
	}
	longName = append(longName, 0)
	if len(longName) != 253 {
		t.Fatalf("unexpected long name length=%d; want 253", len(longName))
	}
	message := []byte{
		0x12, 0x34, 0x01, 0x00,
		0x00, 0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
	message = append(message, longName...)
	message = append(message, 0x00, 0x01, 0x00, 0x01)
	message = append(message, 0x03, 'w', 'w', 'w', 0xc0, 0x0c)
	message = append(message, 0x00, 0x01, 0x00, 0x01)
	if err := validateDNSMessage(message, false); err == nil {
		t.Fatal("expanded compressed DNS name over 255 bytes was accepted")
	}
}
func TestDNSQuestionEqual(t *testing.T) {
	query := testDNSQuery(0x81)
	response := testDNSResponse(query)
	if !dnsQuestionEqual(query, response) {
		t.Fatal("matching question sections reported as different")
	}
	mismatchName := append([]byte(nil), response...)
	mismatchName[13] = 'x'
	if dnsQuestionEqual(query, mismatchName) {
		t.Fatal("response with different question name was accepted")
	}
	mismatchType := append([]byte(nil), response...)
	mismatchType[25], mismatchType[26] = 0x00, 0x02
	if dnsQuestionEqual(query, mismatchType) {
		t.Fatal("response with different question QTYPE was accepted")
	}
	if dnsQuestionEqual(query, query[:20]) {
		t.Fatal("truncated response question was accepted")
	}
}
func TestDNSQuestionEqualIsCaseInsensitive(t *testing.T) {
	query := testDNSQuery(0x71)
	response := testDNSResponse(query)
	response[13] = 'E'
	response[14] = 'X'
	response[15] = 'A'
	response[16] = 'M'
	response[17] = 'P'
	response[18] = 'L'
	response[19] = 'E'
	if !dnsQuestionEqual(query, response) {
		t.Fatal("case-only DNS question difference was rejected")
	}
}
func TestDoHPOSTForwardsValidatedWireMessage(t *testing.T) {
	query := testDNSQuery(0x21)
	response := testDNSResponse(query)
	gotMethod := ""
	var gotBody []byte
	s, cleanup := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/dns-message")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(response)
	})
	defer cleanup()
	rec := postDoH(s, query, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status=%d; want 200, body=%q", rec.Code, rec.Body.String())
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("backend method=%q; want POST", gotMethod)
	}
	if !bytes.Equal(gotBody, query) {
		t.Fatalf("backend body differs from request")
	}
	if got := rec.Header().Get("Content-Type"); got != "application/dns-message" {
		t.Fatalf("response content type=%q; want application/dns-message", got)
	}
	if !bytes.Equal(rec.Body.Bytes(), response) {
		t.Fatal("client response differs from backend DNS response")
	}
}
func TestDoHGETDecodesAndNormalizesToPOST(t *testing.T) {
	query := testDNSQuery(0x31)
	response := testDNSResponse(query)
	gotMethod := ""
	var gotBody []byte
	s, cleanup := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/dns-message; charset=binary")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(response)
	})
	defer cleanup()
	encoded := base64.RawURLEncoding.EncodeToString(query)
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/dns-query?dns="+encoded, nil)
	rec := httptest.NewRecorder()
	s.handleDoH(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status=%d; want 200, body=%q", rec.Code, rec.Body.String())
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("backend method=%q; want POST", gotMethod)
	}
	if !bytes.Equal(gotBody, query) {
		t.Fatalf("decoded GET body differs from DNS query")
	}
}
func TestDoHOptionsHandler(t *testing.T) {
	s, cleanup := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("backend must not be called for OPTIONS")
	})
	defer cleanup()
	req := httptest.NewRequest(http.MethodOptions, "http://gateway.test/dns-query", nil)
	rec := httptest.NewRecorder()
	s.handleDoH(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("OPTIONS status=%d; want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("Access-Control-Allow-Origin=%q; want *", got)
	}
}
func TestGatewayRejectsOtherPaths(t *testing.T) {
	s, cleanup := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("backend must not be called for unknown paths")
	})
	defer cleanup()
	mux := s.routes()
	for _, path := range []string{"/", "/foo", "/dns-query/extra", "/healthz/extra"} {
		req := httptest.NewRequest(http.MethodGet, "http://gateway.test"+path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s status=%d; want 404", path, rec.Code)
		}
	}
}
func TestDoHRateLimitExceeded(t *testing.T) {
	backendCalls := 0
	s, cleanup := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		backendCalls++
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(testDNSResponse(testDNSQuery(0x52)))
	})
	defer cleanup()
	s.limiter = newLimiter(2, time.Minute, 1024)
	query := testDNSQuery(0x52)
	if rec := postDoH(s, query, ""); rec.Code != http.StatusOK {
		t.Fatalf("first request status=%d; want 200", rec.Code)
	}
	if rec := postDoH(s, query, ""); rec.Code != http.StatusOK {
		t.Fatalf("second request status=%d; want 200", rec.Code)
	}
	rec := postDoH(s, query, "")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("third request status=%d; want 429, body=%q", rec.Code, rec.Body.String())
	}
	secs, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil || secs < 59 || secs > 60 {
		t.Fatalf("Retry-After=%q; want about 60 seconds", rec.Header().Get("Retry-After"))
	}
	if backendCalls != 2 {
		t.Fatalf("backend was called %d times; want 2", backendCalls)
	}
}
func TestDoHCapacityReturns503(t *testing.T) {
	backendCalls := 0
	s, cleanup := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		backendCalls++
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(testDNSResponse(testDNSQuery(0x71)))
	})
	defer cleanup()
	s.limiter = newLimiter(defaultRate, defaultWindow, 1)
	query := testDNSQuery(0x71)
	if rec := postDoH(s, query, "192.0.2.1:4000"); rec.Code != http.StatusOK {
		t.Fatalf("first client status=%d; want 200", rec.Code)
	}
	rec := postDoH(s, query, "198.51.100.9:5555")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("new client at capacity status=%d; want 503, body=%q", rec.Code, rec.Body.String())
	}
	if backendCalls != 1 {
		t.Fatalf("backend was called %d times; want 1", backendCalls)
	}
}
func TestDoHConcurrencyLimitReturns503(t *testing.T) {
	backendReached := make(chan struct{}, 1)
	release := make(chan struct{})
	s, cleanup := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		backendReached <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(testDNSResponse(body))
	})
	defer cleanup()
	s.concurrent = make(chan struct{}, 1)
	query := testDNSQuery(0x72)
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- postDoH(s, query, "") }()
	<-backendReached
	second := postDoH(s, query, "")
	if second.Code != http.StatusServiceUnavailable {
		t.Fatalf("second request status=%d; want 503, body=%q", second.Code, second.Body.String())
	}
	close(release)
	first := <-firstDone
	if first.Code != http.StatusOK {
		t.Fatalf("first request status=%d; want 200, body=%q", first.Code, first.Body.String())
	}
}
func TestDoHRejectsOversizedGET(t *testing.T) {
	backendCalls := 0
	s, cleanup := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		backendCalls++
	})
	defer cleanup()
	encoded := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0}, maxDNSBody+1))
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/dns-query?dns="+encoded, nil)
	rec := httptest.NewRecorder()
	s.handleDoH(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d; want 413", rec.Code)
	}
	if backendCalls != 0 {
		t.Fatalf("backend was called %d times for oversized GET; want 0", backendCalls)
	}
}
func TestDoHRejectsInvalidInputBeforeBackend(t *testing.T) {
	backendCalls := 0
	s, cleanup := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		backendCalls++
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(testDNSResponse(testDNSQuery(0x41)))
	})
	defer cleanup()
	cases := []struct {
		name   string
		req    *http.Request
		status int
	}{
		{
			name:   "invalid GET base64",
			req:    httptest.NewRequest(http.MethodGet, "http://gateway.test/dns-query?dns=%%%", nil),
			status: http.StatusBadRequest,
		},
		{
			name: "invalid POST content type",
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodPost, "http://gateway.test/dns-query", bytes.NewReader(testDNSQuery(0x42)))
				r.Header.Set("Content-Type", "text/plain")
				return r
			}(),
			status: http.StatusUnsupportedMediaType,
		},
		{
			name: "invalid DNS wire format",
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodPost, "http://gateway.test/dns-query", bytes.NewReader([]byte{1, 2, 3, 4}))
				r.Header.Set("Content-Type", "application/dns-message")
				return r
			}(),
			status: http.StatusBadRequest,
		},
		{
			name: "unsupported method",
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodPut, "http://gateway.test/dns-query", bytes.NewReader(testDNSQuery(0x43)))
				r.Header.Set("Content-Type", "application/dns-message")
				return r
			}(),
			status: http.StatusMethodNotAllowed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.handleDoH(rec, tc.req)
			if rec.Code != tc.status {
				t.Fatalf("status=%d; want %d, body=%q", rec.Code, tc.status, rec.Body.String())
			}
		})
	}
	if backendCalls != 0 {
		t.Fatalf("backend was called %d times for rejected input; want 0", backendCalls)
	}
}
func TestDoHRejectsOversizedPOST(t *testing.T) {
	backendCalls := 0
	s, cleanup := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		backendCalls++
	})
	defer cleanup()
	body := bytes.Repeat([]byte{0}, maxDNSBody+1)
	req := httptest.NewRequest(http.MethodPost, "http://gateway.test/dns-query", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/dns-message")
	rec := httptest.NewRecorder()
	s.handleDoH(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d; want 413", rec.Code)
	}
	if backendCalls != 0 {
		t.Fatalf("backend was called %d times for oversized input; want 0", backendCalls)
	}
}
func TestDoHRejectsInvalidUpstreamResponse(t *testing.T) {
	query := testDNSQuery(0x51)
	cases := []struct {
		name   string
		status int
		ct     string
		body   []byte
	}{
		{"bad status", http.StatusInternalServerError, "application/dns-message", testDNSResponse(query)},
		{"bad content type", http.StatusOK, "text/plain", testDNSResponse(query)},
		{"bad wire format", http.StatusOK, "application/dns-message", []byte{1, 2, 3, 4}},
		{"transaction id mismatch", http.StatusOK, "application/dns-message", func() []byte {
			b := testDNSResponse(query)
			b[0]++
			return b
		}()},
		{"question mismatch", http.StatusOK, "application/dns-message", func() []byte {
			b := testDNSResponse(query)
			b[13] = 'x'
			return b
		}()},
		{"oversized body", http.StatusOK, "application/dns-message", append(testDNSResponse(query), bytes.Repeat([]byte{0}, maxDNSBody)...)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, cleanup := testServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.ct)
				w.WriteHeader(tc.status)
				_, _ = w.Write(tc.body)
			})
			defer cleanup()
			rec := postDoH(s, query, "")
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status=%d; want 502, body=%q", rec.Code, rec.Body.String())
			}
		})
	}
}
func TestDoHUpstreamTimeoutReturns504(t *testing.T) {
	query := testDNSQuery(0x64)
	s, cleanup := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(testDNSResponse(query))
	})
	defer cleanup()
	s.client = &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 50 * time.Millisecond}}
	rec := postDoH(s, query, "")
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status=%d; want 504, body=%q", rec.Code, rec.Body.String())
	}
}
func TestDoHUpstreamConnectionFailureReturns502(t *testing.T) {
	query := testDNSQuery(0x65)
	s, cleanup := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("backend must not be called")
	})
	defer cleanup()
	s.backendURL = "http://127.0.0.1:1/dns-query"
	rec := postDoH(s, query, "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d; want 502, body=%q", rec.Code, rec.Body.String())
	}
}
func TestDoHRejectsOversizedDeclaredUpstreamContentLength(t *testing.T) {
	query := testDNSQuery(0x63)
	s, cleanup := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/dns-message")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", maxDNSBody+1))
		w.WriteHeader(http.StatusOK)
	})
	defer cleanup()
	rec := postDoH(s, query, "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d; want 502", rec.Code)
	}
}
func TestDoHRejectsOversizedUpstreamHeaders(t *testing.T) {
	query := testDNSQuery(0x62)
	s, cleanup := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Huge", string(bytes.Repeat([]byte{'x'}, maxHeaderBytes+1024)))
		w.Header().Set("Content-Type", "application/dns-message")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(testDNSResponse(query))
	})
	defer cleanup()
	transport := &http.Transport{MaxResponseHeaderBytes: maxHeaderBytes}
	s.client = &http.Client{Transport: transport}
	rec := postDoH(s, query, "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d; want 502", rec.Code)
	}
}
func TestDoHDoesNotFollowBackendRedirects(t *testing.T) {
	query := testDNSQuery(0x61)
	called := 0
	s, cleanup := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		called++
		http.Redirect(w, r, "/unexpected", http.StatusFound)
	})
	defer cleanup()
	transport := &http.Transport{
		MaxIdleConns:           4,
		MaxIdleConnsPerHost:    4,
		MaxConnsPerHost:        4,
		MaxResponseHeaderBytes: maxHeaderBytes,
	}
	s.client = &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	rec := postDoH(s, query, "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d; want 502", rec.Code)
	}
	if called != 1 {
		t.Fatalf("backend was called %d times; want exactly 1", called)
	}
}
func TestGatewayHealthReadyWhenBackendListenerAccepts(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	s := &server{backendAddr: ln.Addr().String()}
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/healthz", nil)
	rec := httptest.NewRecorder()
	s.health(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("health status=%d; want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "ok" {
		t.Fatalf("health body=%q; want ok", got)
	}
}
func TestGatewayHealthRequiresBackend(t *testing.T) {
	s := &server{backendAddr: "127.0.0.1:1"}
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/healthz", nil)
	rec := httptest.NewRecorder()
	s.health(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("health status=%d; want 503", rec.Code)
	}
}
func TestClientIPUsesRightmostValidForwardedAddress(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/healthz", nil)
	req.RemoteAddr = "192.0.2.10:1234"
	req.Header.Set("X-Forwarded-For", "198.51.100.7, unknown, 203.0.113.9")
	if got := clientIP(req, true); got != "203.0.113.9" {
		t.Fatalf("clientIP=%q; want 203.0.113.9", got)
	}
}
func TestClientIPUsesRightmostAcrossMultipleForwardedHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/healthz", nil)
	req.RemoteAddr = "192.0.2.10:1234"
	// A client-supplied header line followed by a separate line appended by
	// the trusted proxy: the proxy's (rightmost) address must win.
	req.Header.Add("X-Forwarded-For", "198.51.100.7")
	req.Header.Add("X-Forwarded-For", "203.0.113.9")
	if got := clientIP(req, true); got != "203.0.113.9" {
		t.Fatalf("clientIP=%q; want 203.0.113.9", got)
	}
}
func TestClientIPNormalizesIPv6(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/healthz", nil)
	req.RemoteAddr = "[2001:0db8:0000:0000:0000:0000:0000:0001]:443"
	if got := clientIP(req, false); got != "2001:db8::1" {
		t.Fatalf("clientIP=%q; want 2001:db8::1", got)
	}
	req.Header.Set("X-Forwarded-For", "2001:0DB8:0000::0002")
	if got := clientIP(req, true); got != "2001:db8::2" {
		t.Fatalf("clientIP=%q; want 2001:db8::2", got)
	}
}
func TestClientIPDoesNotTrustForwardedHeadersWhenDisabled(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/healthz", nil)
	req.RemoteAddr = "192.0.2.10:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	if got := clientIP(req, false); got != "192.0.2.10" {
		t.Fatalf("clientIP=%q; want remote peer address", got)
	}
}
func TestParseEnvIntRejectsUnsafeValues(t *testing.T) {
	cases := []struct {
		name  string
		value string
		def   int
		max   int
		want  int
	}{
		{"valid", "12", 99, 256, 12},
		{"empty", "", 99, 256, 99},
		{"invalid", "abc", 99, 256, 99},
		{"zero", "0", 99, 256, 99},
		{"negative", "-1", 99, 256, 99},
		{"above cap", "257", 99, 256, 99},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TEST_INT", tc.value)
			if got := parseEnvInt("TEST_INT", tc.def, tc.max); got != tc.want {
				t.Fatalf("parseEnvInt(%q, %d, %d)=%d; want %d", tc.value, tc.def, tc.max, got, tc.want)
			}
		})
	}
}
func TestParseEnvBoolRejectsInvalidValues(t *testing.T) {
	cases := []struct {
		name  string
		value string
		def   bool
		want  bool
	}{
		{"true", "true", false, true},
		{"false", "false", true, false},
		{"mixed-case true", "TrUe", false, true},
		{"mixed-case false", "FaLsE", true, false},
		{"invalid", "yes", false, false},
		{"invalid with true default", "maybe", true, true},
		{"empty uses default", "", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TEST_BOOL", tc.value)
			if got := parseEnvBool("TEST_BOOL", tc.def); got != tc.want {
				t.Fatalf("parseEnvBool(%q, %v)=%v; want %v", tc.value, tc.def, got, tc.want)
			}
		})
	}
}
func TestValidContentType(t *testing.T) {
	cases := map[string]bool{
		"application/dns-message":                 true,
		"application/dns-message; charset=binary": true,
		"Application/DNS-Message; charset=binary": true,
		"text/plain": false,
		"application/dns-message; invalid parameter": false,
	}
	for value, want := range cases {
		t.Run(fmt.Sprintf("%q", value), func(t *testing.T) {
			if got := validContentType(value); got != want {
				t.Fatalf("validContentType(%q)=%v; want %v", value, got, want)
			}
		})
	}
}
func TestDoHInvalidRequestsDoNotConsumeRateBudget(t *testing.T) {
	backendCalls := 0
	s, cleanup := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		backendCalls++
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(testDNSResponse(testDNSQuery(0x91)))
	})
	defer cleanup()
	s.limiter = newLimiter(2, time.Minute, 1024)
	query := testDNSQuery(0x91)
	if rec := postDoH(s, query, ""); rec.Code != http.StatusOK {
		t.Fatalf("first valid request status=%d; want 200", rec.Code)
	}
	badReq := httptest.NewRequest(http.MethodPost, "http://gateway.test/dns-query", bytes.NewReader([]byte{1, 2, 3, 4}))
	badReq.Header.Set("Content-Type", "application/dns-message")
	rec := httptest.NewRecorder()
	s.handleDoH(rec, badReq)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid request status=%d; want 400", rec.Code)
	}
	// The rejected request must not have charged the client's window.
	if rec := postDoH(s, query, ""); rec.Code != http.StatusOK {
		t.Fatalf("second valid request status=%d; want 200 (invalid request consumed rate budget)", rec.Code)
	}
	if rec := postDoH(s, query, ""); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("third valid request status=%d; want 429", rec.Code)
	}
	if backendCalls != 2 {
		t.Fatalf("backend called %d times; want 2", backendCalls)
	}
}

func TestLimiterRefreshesSeenOnRateLimitedRequests(t *testing.T) {
	// The window (5m) outlasts the idle TTL (2m) so the second request is
	// still rate limited when it arrives after the TTL.
	l := newLimiter(1, 5*time.Minute, 2)
	now := time.Unix(7_000_000, 0)
	if got := l.check("198.51.100.1", now); got != limitAllowed {
		t.Fatalf("first request got %v; want allowed", got)
	}
	rejected := now.Add(150 * time.Second) // past the 2-minute idle TTL
	if got := l.check("198.51.100.1", rejected); got != limitRateLimited {
		t.Fatalf("second request got %v; want rate limited", got)
	}
	if got := l.clients["198.51.100.1"].seen; !got.Equal(rejected) {
		t.Fatalf("seen=%v; want %v refreshed by the rejected request", got, rejected)
	}
}

func TestLimiterKeepsPersistentlyRateLimitedClientAtCapacity(t *testing.T) {
	// A 10-minute window keeps the first timestamp live for the whole test.
	l := newLimiter(1, 10*time.Minute, 1)
	start := time.Unix(8_000_000, 0)
	if got := l.check("198.51.100.1", start); got != limitAllowed {
		t.Fatalf("first request got %v; want allowed", got)
	}
	last := start
	// Six 30s steps end 180s after the first request, beyond the 2-minute idle
	// TTL, so the client would be evictable had its seen time not been
	// refreshed by the rejected requests.
	for i := 0; i < 6; i++ {
		last = last.Add(30 * time.Second)
		if got := l.check("198.51.100.1", last); got != limitRateLimited {
			t.Fatalf("follow-up request %d got %v; want rate limited", i+2, got)
		}
	}
	// The throttled-but-active client must not be evictable as idle.
	if got := l.check("198.51.100.2", last); got != limitCapacity {
		t.Fatalf("new client got %v; want limitCapacity", got)
	}
}

func TestDoHRejectsOversizedGETQueryStringBeforeParsing(t *testing.T) {
	backendCalls := 0
	s, cleanup := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		backendCalls++
	})
	defer cleanup()
	encoded := base64.RawURLEncoding.EncodeToString(testDNSQuery(0x92))
	// Valid dns= parameter, but the raw query string as a whole exceeds the
	// maximum legitimate size; it must be rejected as 413 without parsing
	// and without touching the backend.
	junk := strings.Repeat("x", len("dns=")+base64.RawURLEncoding.EncodedLen(maxDNSBody))
	rawQuery := "dns=" + encoded + "&pad=" + junk
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/dns-query", nil)
	req.URL.RawQuery = rawQuery
	rec := httptest.NewRecorder()
	s.handleDoH(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d; want 413, body=%q", rec.Code, rec.Body.String())
	}
	if backendCalls != 0 {
		t.Fatalf("backend was called %d times; want 0", backendCalls)
	}
}

func TestDoHHeadRequestIsRejected(t *testing.T) {
	s, cleanup := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("backend must not be called for HEAD")
	})
	defer cleanup()
	encoded := base64.RawURLEncoding.EncodeToString(testDNSQuery(0x93))
	req := httptest.NewRequest(http.MethodHead, "http://gateway.test/dns-query?dns="+encoded, nil)
	rec := httptest.NewRecorder()
	s.handleDoH(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("HEAD status=%d; want 405, body=%q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Allow"); got != "GET, POST, OPTIONS" {
		t.Fatalf("Allow=%q; want GET, POST, OPTIONS", got)
	}
}
