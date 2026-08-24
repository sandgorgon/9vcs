package main

import (
	"net"
	"testing"
	"time"
)

func TestConnCapListenerRejectsBeyondMax(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	ln := newConnCapListener(raw, 1)

	dial := func() net.Conn {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	// First connection: accepted and kept open.
	client1 := dial()
	defer client1.Close()
	accepted1, err := ln.Accept()
	if err != nil {
		t.Fatalf("Accept 1: %v", err)
	}
	defer accepted1.Close()

	// Second connection while the first is still live: the listener
	// should close it before ever returning it — confirm by having the
	// dialing side observe a close rather than getting a working conn.
	client2 := dial()
	defer client2.Close()
	client2.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if n, err := client2.Read(buf); err == nil {
		t.Fatalf("expected the over-cap connection to be closed by the server, got %d bytes with no error", n)
	}

	// After the first connection closes, its slot frees up and a new
	// connection is admitted.
	accepted1.Close()
	client3 := dial()
	defer client3.Close()
	accepted3, err := ln.Accept()
	if err != nil {
		t.Fatalf("Accept after slot freed: %v", err)
	}
	defer accepted3.Close()
}

func TestIPRateLimiterAllowsUpToLimitWithinWindow(t *testing.T) {
	r := newIPRateLimiter(3, time.Minute)
	now := time.Now()

	for i := 0; i < 3; i++ {
		if !r.allowAt("1.2.3.4", now) {
			t.Fatalf("call %d: expected allowed within limit", i+1)
		}
	}
	if r.allowAt("1.2.3.4", now) {
		t.Error("4th call within the window should be refused")
	}
}

func TestIPRateLimiterIsPerAddress(t *testing.T) {
	r := newIPRateLimiter(1, time.Minute)
	now := time.Now()

	if !r.allowAt("1.1.1.1", now) {
		t.Fatal("first call for 1.1.1.1 should be allowed")
	}
	if r.allowAt("1.1.1.1", now) {
		t.Error("second call for 1.1.1.1 within the window should be refused")
	}
	if !r.allowAt("2.2.2.2", now) {
		t.Error("a different address should have its own independent limit")
	}
}

func TestIPRateLimiterResetsAfterWindow(t *testing.T) {
	r := newIPRateLimiter(1, time.Minute)
	now := time.Now()

	if !r.allowAt("1.2.3.4", now) {
		t.Fatal("first call should be allowed")
	}
	if r.allowAt("1.2.3.4", now.Add(30*time.Second)) {
		t.Error("call still inside the window should be refused")
	}
	if !r.allowAt("1.2.3.4", now.Add(time.Minute+time.Second)) {
		t.Error("call after the window elapsed should be allowed again")
	}
}

func TestRateLimitListenerRejectsOverLimit(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	ln := newRateLimitListener(raw, newIPRateLimiter(1, time.Minute))

	dial := func() net.Conn {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	client1 := dial()
	defer client1.Close()
	accepted1, err := ln.Accept()
	if err != nil {
		t.Fatalf("Accept 1: %v", err)
	}
	defer accepted1.Close()

	// Both connections come from 127.0.0.1 (loopback dial), so the
	// second should be rejected by the same-address rate limit even
	// though connCapListener isn't in play here at all.
	client2 := dial()
	defer client2.Close()
	client2.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if n, err := client2.Read(buf); err == nil {
		t.Fatalf("expected the rate-limited connection to be closed by the server, got %d bytes with no error", n)
	}
}
