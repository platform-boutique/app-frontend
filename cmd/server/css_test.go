package main

import (
	"math"
	"os"
	"regexp"
	"strconv"
	"testing"
)

func luminance(hex string) float64 {
	ch := func(i int) float64 {
		v, _ := strconv.ParseUint(hex[i:i+2], 16, 8)
		c := float64(v) / 255
		if c <= 0.03928 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	return 0.2126*ch(1) + 0.7152*ch(3) + 0.0722*ch(5)
}

func contrast(a, b string) float64 {
	la, lb := luminance(a), luminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// WCAG 1.4.11: the border of form controls must have 3:1 against what is around it,
// in the light theme and in both dark declarations (OS preference and explicit choice).
func TestControlBorderContrast(t *testing.T) {
	css, err := os.ReadFile("../../static/styles/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	token := func(name string) []string {
		var out []string
		for _, m := range regexp.MustCompile(`--color-`+name+`:\s*(#[0-9a-fA-F]{6})`).FindAllStringSubmatch(string(css), -1) {
			out = append(out, m[1])
		}
		return out
	}
	border, bg, surface, surface2 := token("control-border"), token("bg"), token("surface"), token("surface-2")
	if len(border) != 3 || len(bg) != 3 {
		t.Fatalf("want light + 2 dark declarations, got %d borders, %d backgrounds", len(border), len(bg))
	}
	for i, name := range []string{"light", "dark (OS)", "dark (explicit)"} {
		for what, others := range map[string][]string{"page": bg, "surface": surface, "surface-2": surface2} {
			if got := contrast(border[i], others[i]); got < 3 {
				t.Errorf("%s: control border %s on %s %s is %.2f:1, want >= 3:1", name, border[i], what, others[i], got)
			}
		}
	}
	for _, rule := range []string{`\.input, \.select \{[^}]*border: 1px solid var\(--color-control-border\)`, `\.btn-secondary \{[^}]*border-color: var\(--color-control-border\)`} {
		if !regexp.MustCompile(rule).Match(css) {
			t.Errorf("rule does not use --color-control-border: %s", rule)
		}
	}
}
