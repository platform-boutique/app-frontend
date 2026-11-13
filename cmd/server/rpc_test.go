package main

import (
	"testing"

	pb "frontend/genproto"
)

// ---------------------------------------------------------------------------
// cartIDs
// ---------------------------------------------------------------------------

func TestCartIDs_Empty(t *testing.T) {
	got := cartIDs([]*pb.CartItem{})
	if len(got) != 0 {
		t.Errorf("expected empty slice, got %v", got)
	}
}

func TestCartIDs_SingleItem(t *testing.T) {
	items := []*pb.CartItem{
		{ProductId: "abc123", Quantity: 1},
	}
	got := cartIDs(items)
	if len(got) != 1 {
		t.Fatalf("expected 1 item, got %d", len(got))
	}
	if got[0] != "abc123" {
		t.Errorf("expected abc123, got %s", got[0])
	}
}

func TestCartIDs_MultipleItems(t *testing.T) {
	items := []*pb.CartItem{
		{ProductId: "abc001", Quantity: 1},
		{ProductId: "abc002", Quantity: 2},
		{ProductId: "abc003", Quantity: 3},
	}
	got := cartIDs(items)
	if len(got) != 3 {
		t.Fatalf("expected 3 items, got %d", len(got))
	}
	expected := []string{"abc001", "abc002", "abc003"}
	for i, id := range expected {
		if got[i] != id {
			t.Errorf("index %d: expected %s, got %s", i, id, got[i])
		}
	}
}

// ---------------------------------------------------------------------------
// cartSize
// ---------------------------------------------------------------------------

func TestCartSize_Empty(t *testing.T) {
	got := cartSize([]*pb.CartItem{})
	if got != 0 {
		t.Errorf("expected 0, got %d", got)
	}
}

func TestCartSize_SingleItem(t *testing.T) {
	items := []*pb.CartItem{
		{ProductId: "abc001", Quantity: 3},
	}
	got := cartSize(items)
	if got != 3 {
		t.Errorf("expected 3, got %d", got)
	}
}

func TestCartSize_MultipleItems(t *testing.T) {
	items := []*pb.CartItem{
		{ProductId: "abc001", Quantity: 2},
		{ProductId: "abc002", Quantity: 5},
		{ProductId: "abc003", Quantity: 1},
	}
	got := cartSize(items)
	if got != 8 {
		t.Errorf("expected 8, got %d", got)
	}
}

// ---------------------------------------------------------------------------
// renderMoney
// ---------------------------------------------------------------------------

func TestRenderMoney_USD(t *testing.T) {
	m := &pb.Money{CurrencyCode: "USD", Units: 10, Nanos: 990000000}
	got := renderMoney(m)
	want := "$10.99"
	if got != want {
		t.Errorf("expected %s, got %s", want, got)
	}
}

func TestRenderMoney_EUR(t *testing.T) {
	m := &pb.Money{CurrencyCode: "EUR", Units: 5, Nanos: 500000000}
	got := renderMoney(m)
	want := "€5.50"
	if got != want {
		t.Errorf("expected %s, got %s", want, got)
	}
}

func TestRenderMoney_JPY(t *testing.T) {
	m := &pb.Money{CurrencyCode: "JPY", Units: 1000, Nanos: 0}
	got := renderMoney(m)
	want := "¥1000"
	if got != want {
		t.Errorf("expected %s, got %s", want, got)
	}
}

func TestRenderMoney_GBP(t *testing.T) {
	m := &pb.Money{CurrencyCode: "GBP", Units: 20, Nanos: 0}
	got := renderMoney(m)
	want := "£20.00"
	if got != want {
		t.Errorf("expected %s, got %s", want, got)
	}
}

func TestRenderMoney_TRY(t *testing.T) {
	m := &pb.Money{CurrencyCode: "TRY", Units: 100, Nanos: 0}
	got := renderMoney(m)
	want := "₺100.00"
	if got != want {
		t.Errorf("expected %s, got %s", want, got)
	}
}

func TestRenderMoney_UnknownCurrency(t *testing.T) {
	m := &pb.Money{CurrencyCode: "CHF", Units: 10, Nanos: 0}
	got := renderMoney(m)
	want := "$10.00"
	if got != want {
		t.Errorf("expected %s (default $), got %s", want, got)
	}
}

func TestRenderMoney_ZeroValue(t *testing.T) {
	m := &pb.Money{CurrencyCode: "USD", Units: 0, Nanos: 0}
	got := renderMoney(m)
	want := "$0.00"
	if got != want {
		t.Errorf("expected %s, got %s", want, got)
	}
}

// ---------------------------------------------------------------------------
// renderCurrencyLogo
// ---------------------------------------------------------------------------

func TestRenderCurrencyLogo(t *testing.T) {
	tests := []struct {
		code string
		want string
	}{
		{"USD", "$"},
		{"CAD", "$"},
		{"EUR", "€"},
		{"JPY", "¥"},
		{"GBP", "£"},
		{"TRY", "₺"},
		{"CHF", "$"}, // unknown → default
		{"", "$"},    // empty → default
	}
	for _, tt := range tests {
		got := renderCurrencyLogo(tt.code)
		if got != tt.want {
			t.Errorf("renderCurrencyLogo(%q) = %q, want %q", tt.code, got, tt.want)
		}
	}
}
