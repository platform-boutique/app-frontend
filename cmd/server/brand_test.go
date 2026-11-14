package main

import "testing"

func TestBrandFromEnv_Defaults(t *testing.T) {
	t.Setenv("BRAND_NAME", "")
	t.Setenv("BRAND_TAGLINE", "   ")
	b := brandFromEnv()
	if b.Name != defaultBrandName || b.Tagline != defaultBrandTagline {
		t.Errorf("got %+v, want defaults", b)
	}
}

func TestBrandFromEnv_Override(t *testing.T) {
	t.Setenv("BRAND_NAME", " Acme Goods ")
	t.Setenv("BRAND_TAGLINE", "Things you need.")
	b := brandFromEnv()
	if b.Name != "Acme Goods" || b.Tagline != "Things you need." {
		t.Errorf("got %+v", b)
	}
}
