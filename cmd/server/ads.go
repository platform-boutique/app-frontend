package main

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"

	pb "frontend/genproto"
)

// Ads: which page asks for which ads, the click-tracking link and its redirect.
// Ads are optional everywhere: no ad-service, no ads, never an error page.

// RecordClick is best effort: the visitor waits for it at most this long. A variable
// only so tests can shorten it.
var adClickTimeout = time.Second

const (
	maxAdSearchWords = 5
	maxAdIDLen       = 64
	maxRedirectLen   = 2048
)

// adContextKeys are the context_keys of GetAds for a browse view: the department id on
// department pages and the search words (lower case, split on whitespace, at most
// maxAdSearchWords) on search pages. Neither on the landing page: the ad-service then
// picks any ad. The product page passes the product's categories itself.
func adContextKeys(p browseParams) []string {
	keys := []string{}
	if p.Category != "" {
		keys = append(keys, p.Category)
	}
	return append(keys, searchWords(p.Query)...)
}

// searchWords splits a search text into at most maxAdSearchWords lower-case words.
func searchWords(q string) []string {
	words := strings.Fields(strings.ToLower(q))
	if len(words) > maxAdSearchWords {
		words = words[:maxAdSearchWords]
	}
	return words
}

// adHref is the link of an ad: through the click counter when the ad has an id,
// straight to its target when it has none (an ad-service from before click tracking).
func adHref(base string, ad *pb.Ad) string {
	if ad.GetId() == "" {
		return base + ad.GetRedirectUrl()
	}
	return base + "/ads/" + url.PathEscape(ad.GetId()) + "/click?to=" + url.QueryEscape(ad.GetRedirectUrl())
}

// isLocalPath is true only for a path on this site: it starts with exactly one "/"
// (not "//", not "/\", which browsers read as another host), is valid UTF-8, free of
// control characters (a tab or newline inside "/\t/evil.com" is dropped by browsers and
// leaves "//evil.com") and parses as a URL without scheme or host.
func isLocalPath(s string) bool {
	if s == "" || len(s) > maxRedirectLen || s[0] != '/' || !utf8.ValidString(s) {
		return false
	}
	if len(s) > 1 && (s[1] == '/' || s[1] == '\\') {
		return false
	}
	for _, c := range s {
		if unicode.IsControl(c) {
			return false
		}
	}
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "" && u.Host == "" && u.Opaque == "" && u.User == nil
}

func validAdID(id string) bool {
	if id == "" || len(id) > maxAdIDLen {
		return false
	}
	for _, c := range id {
		letter := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
		if !letter && (c < '0' || c > '9') && c != '-' && c != '_' && c != '.' {
			return false
		}
	}
	return true
}

// recordAdClick tells ad-service about a click. Failures are only logged.
func (fe *frontendServer) recordAdClick(ctx context.Context, log logrus.FieldLogger, id string) {
	if fe.adSvcConn == nil || !validAdID(id) {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, adClickTimeout)
	defer cancel()
	if _, err := pb.NewAdServiceClient(fe.adSvcConn).RecordClick(ctx, &pb.RecordClickRequest{AdId: id}); err != nil {
		log.WithField("error", err).WithField("ad", id).Debug("could not record the ad click")
	}
}

// adClickHandler counts the click and sends the visitor on. The target comes from the
// URL, so it is followed only when it is a local path (no open redirect); anything else
// goes to the home page. The click is counted either way.
func (fe *frontendServer) adClickHandler(w http.ResponseWriter, r *http.Request) {
	log := r.Context().Value(ctxKeyLog{}).(logrus.FieldLogger)
	fe.recordAdClick(r.Context(), log, mux.Vars(r)["id"])
	to := r.URL.Query().Get("to")
	if !isLocalPath(to) {
		to = "/"
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Location", baseUrl+to)
	w.WriteHeader(http.StatusFound)
}
