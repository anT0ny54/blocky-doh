package main

import (
	"testing"
	"time"
)

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
