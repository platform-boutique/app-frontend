package main

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// CODE-07: the header-nav cache reads time through an injected clock, so its TTLs are tested by
// moving the clock, not by sleeping (and a jump of the host clock cannot make these tests flaky).
func TestNavCache_TTLFollowsTheInjectedClock(t *testing.T) {
	cat := sampleCatalog()
	fe := newFakeFrontend(t, cat)
	clk := newTestClock()
	fe.nav.now = clk.Now
	ctx := context.Background()

	if got := fe.navCategories(ctx); len(got) != 2 || cat.listCalls.Load() != 1 {
		t.Fatalf("first call: %d categories, %d fetches", len(got), cat.listCalls.Load())
	}
	clk.Advance(navCacheTTL - time.Second)
	fe.navCategories(ctx)
	if n := cat.listCalls.Load(); n != 1 {
		t.Fatalf("a fetch inside the TTL: %d fetches", n)
	}
	clk.Advance(2 * time.Second) // now past the TTL
	fe.navCategories(ctx)
	if n := cat.listCalls.Load(); n != 2 {
		t.Fatalf("no refetch after the TTL: %d fetches", n)
	}
}

func TestNavCache_ErrorKeepsLastListAndRetriesAfterTheShortTTL(t *testing.T) {
	cat := sampleCatalog()
	fe := newFakeFrontend(t, cat)
	clk := newTestClock()
	fe.nav.now = clk.Now
	ctx := context.Background()

	first := fe.navCategories(ctx)
	if len(first) != 2 {
		t.Fatalf("setup: %d categories", len(first))
	}
	clk.Advance(navCacheTTL + time.Second)
	cat.listErr = status.Error(codes.Unavailable, "catalog down")

	if got := fe.navCategories(ctx); len(got) != 2 {
		t.Fatalf("a failed refresh must keep the last known list, got %d entries", len(got))
	}
	failed := cat.listCalls.Load()
	clk.Advance(navCacheErrTTL - time.Second)
	fe.navCategories(ctx)
	if n := cat.listCalls.Load(); n != failed {
		t.Fatalf("retried inside the error TTL: %d fetches, want %d", n, failed)
	}
	cat.listErr = nil
	clk.Advance(2 * time.Second) // past the error TTL
	fe.navCategories(ctx)
	if n := cat.listCalls.Load(); n != failed+1 {
		t.Fatalf("no retry after the error TTL: %d fetches, want %d", n, failed+1)
	}
}
