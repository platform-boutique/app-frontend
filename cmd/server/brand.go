package main

import (
	"os"
	"strings"
)

// Default store identity. The name is a working name; set BRAND_NAME and
// BRAND_TAGLINE to rename the store without touching templates or styles.
const (
	defaultBrandName    = "Everly"
	defaultBrandTagline = "Everyday things, thoughtfully chosen."
)

// brandConfig is the single configuration point for the store's identity.
// Every template receives it as brand_name and brand_tagline.
type brandConfig struct {
	Name    string
	Tagline string
}

// brandFromEnv reads BRAND_NAME and BRAND_TAGLINE; empty or blank values fall
// back to the defaults.
func brandFromEnv() brandConfig {
	b := brandConfig{Name: defaultBrandName, Tagline: defaultBrandTagline}
	if v := strings.TrimSpace(os.Getenv("BRAND_NAME")); v != "" {
		b.Name = v
	}
	if v := strings.TrimSpace(os.Getenv("BRAND_TAGLINE")); v != "" {
		b.Tagline = v
	}
	return b
}
