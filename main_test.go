package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"
)

func testDNSQuery(id byte) []byte {
	return []byte{
		id, 0x34, 0x01, 0x00, // ID, RD
		0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // counts
		0x07, 'e', 'x', 'a', 'm', 'p', 'l', 'e',
		0x03, 'c', 'o', 'm', 0x00,
		0x00, 0x01, 0x00, 0x01, // A / IN
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

func TestLimiterAllows99ThenRejects100thImmediateRequest(t *testing.T) {
	l := newLimiter(99, 60*time.Second, 1024)
	now := time.Unix(1_000_000, 0)
	for i := 0; i < 99; i++ {
		if !l.allow("203.0.113.10", now) {
			t.Fatalf("request %d was rejected; want first 99 requests allowed", i+1)
		}
	}
	if l.allow("203.0.113.10", now) {
		t.Fatal("100th immediate request was allowed; want rate limiting")
	}
}

func TestLimiterSeparatesClientIPs(t *testing.T) {
	l := newLimiter(99, 60*time.Second, 1024)
	now := time.Unix(1_000_000, 0)
	for i := 0; i < 99; i++ {
		_ = l.allow("203.0.113.10", now)
	}
	if !l.allow("198.51.100.20", now) {
		t.Fatal("second client IP was throttled by first client IP")
	}
}

func TestLimiterConcurrentAccessIsBoundedPerClient(t *testing.T) {
	l := newLimiter(99, 60*time.Second, 1024)
	now := time.Unix(1_000_000, 0)
	const goroutines = 32
	const attempts = 8

	results := make(chan bool, goroutines*attempts)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < attempts; j++ {
				results <- l.allow("203.0.113.10", now)
			}
		}()
	}
	wg.Wait()
	close(results)

	allowed := 0
	for ok := range results {
		if ok {
			allowed++
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
	if !l.allow("203.0.113.10", now) {
		t.Fatal("first request should be allowed")
	}
	if l.clients == nil {
		t.Fatal("limiter client map was not allocated on first request")
	}
}

func TestLimiterDoesNotEvictLiveStateAtCapacity(t *testing.T) {
	l := newLimiter(1, time.Minute, 1024)
	l.maxClients = 2
	now := time.Now()
	if !l.allow("198.51.100.1", now) {
		t.Fatal("first client should be allowed")
	}
	if !l.allow("198.51.100.2", now) {
		t.Fatal("second client should be allowed")
	}
	if l.allow("198.51.100.3", now) {
		t.Fatal("new client should be rejected when all states are live")
	}
	if l.allow("198.51.100.1", now) {
		t.Fatal("existing client's limiter state was split or reset")
	}
}

func TestLimiterEvictsStaleStateAtCapacity(t *testing.T) {
	l := newLimiter(1, time.Minute, 1024)
	l.maxClients = 2
	l.clients = make(map[string]*clientState)
	now := time.Now()
	l.clients["198.51.100.1"] = &clientState{tat: now.Add(-time.Minute), seen: now.Add(-clientIdleTTL - time.Second)}
	l.clients["198.51.100.2"] = &clientState{tat: now, seen: now}
	if !l.allow("198.51.100.3", now) {
		t.Fatal("new client should be allowed after stale-state eviction")
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
			0x00,                   // root owner name
			0x00, 0x01, 0x00, 0x01, // A / IN
			0x00, 0x00, 0x00, 0x00, // TTL
			0x00, 0x00, // empty RDATA
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

	req := httptest.NewRequest(http.MethodPost, "http://gateway.test/dns-query", bytes.NewReader(query))
	req.Header.Set("Content-Type", "application/dns-message")
	rec := httptest.NewRecorder()
	s.handleDoH(rec, req)

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

			req := httptest.NewRequest(http.MethodPost, "http://gateway.test/dns-query", bytes.NewReader(query))
			req.Header.Set("Content-Type", "application/dns-message")
			rec := httptest.NewRecorder()
			s.handleDoH(rec, req)
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status=%d; want 502, body=%q", rec.Code, rec.Body.String())
			}
		})
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

	req := httptest.NewRequest(http.MethodPost, "http://gateway.test/dns-query", bytes.NewReader(query))
	req.Header.Set("Content-Type", "application/dns-message")
	rec := httptest.NewRecorder()
	s.handleDoH(rec, req)
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

	req := httptest.NewRequest(http.MethodPost, "http://gateway.test/dns-query", bytes.NewReader(query))
	req.Header.Set("Content-Type", "application/dns-message")
	rec := httptest.NewRecorder()
	s.handleDoH(rec, req)

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

	req := httptest.NewRequest(http.MethodPost, "http://gateway.test/dns-query", bytes.NewReader(query))
	req.Header.Set("Content-Type", "application/dns-message")
	rec := httptest.NewRecorder()
	s.handleDoH(rec, req)

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

func TestClientIPUsesRightmostValidForwardedAddress(t *testing.T) {
	previous := os.Getenv("TRUST_PROXY")
	if err := os.Setenv("TRUST_PROXY", "true"); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Setenv("TRUST_PROXY", previous) }()

	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/healthz", nil)
	req.RemoteAddr = "192.0.2.10:1234"
	req.Header.Set("X-Forwarded-For", "198.51.100.7, unknown, 203.0.113.9")
	if got := clientIP(req, true); got != "203.0.113.9" {
		t.Fatalf("clientIP=%q; want 203.0.113.9", got)
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

func TestGatewayHealthRequiresBackend(t *testing.T) {
	s := &server{backendAddr: "127.0.0.1:1"}
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/healthz", nil)
	rec := httptest.NewRecorder()
	s.health(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("health status=%d; want 503", rec.Code)
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
