package main

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "frontend/genproto"
)

// Slice 7: rating bars, ad context keys and click redirect, recommendation inputs.

// ---------------------------------------------------------------------------
// rating bars
// ---------------------------------------------------------------------------

func TestBars_Arithmetic(t *testing.T) {
	rv := ratingView{Average: 4.2, Count: 12, Stars: [5]int32{1, 1, 2, 3, 5}, HasStars: true}
	bars := rv.Bars()
	if len(bars) != 5 {
		t.Fatalf("want 5 bars, got %d", len(bars))
	}
	want := []ratingBar{
		{5, 5, 41.7, "5 reviews rated 5 stars"},
		{4, 3, 25, "3 reviews rated 4 stars"},
		{3, 2, 16.7, "2 reviews rated 3 stars"},
		{2, 1, 8.3, "1 review rated 2 stars"},
		{1, 1, 8.3, "1 review rated 1 star"},
	}
	for i, w := range want {
		if bars[i] != w {
			t.Errorf("bar %d: got %+v, want %+v", i, bars[i], w)
		}
	}
	if got := bars[0].Width(); got != "41.7%" {
		t.Errorf("width %q", got)
	}
	if got := bars[1].Width(); got != "25%" {
		t.Errorf("width %q", got)
	}
	// all reviews with one rating: a full bar, empty bars at 0 (rows are never dropped)
	one := ratingView{Count: 3, Stars: [5]int32{0, 0, 0, 0, 3}, HasStars: true}.Bars()
	if one[0].Width() != "100%" || one[1].Width() != "0%" || one[4].Label != "0 reviews rated 1 star" {
		t.Errorf("single-rating distribution: %+v", one)
	}
}

func TestBars_NoneWithoutDistribution(t *testing.T) {
	for name, rv := range map[string]ratingView{
		"no star_counts": {Average: 4, Count: 3},
		"no reviews":     {HasStars: true},
	} {
		if got := rv.Bars(); got != nil {
			t.Errorf("%s: want no bars, got %v", name, got)
		}
	}
}

func TestStarsFromCounts(t *testing.T) {
	cases := []struct {
		name   string
		counts []int32
		count  int32
		ok     bool
	}{
		{"consistent", []int32{1, 0, 0, 0, 1}, 2, true},
		{"zeros and zero count", []int32{0, 0, 0, 0, 0}, 0, false}, // no reviews: nothing to draw
		{"missing (older service)", nil, 4, false},
		{"four entries", []int32{1, 1, 1, 1}, 4, false},
		{"six entries", []int32{1, 1, 1, 1, 0, 0}, 4, false},
		{"sum differs from count", []int32{1, 1, 1, 1, 1}, 4, false},
		{"negative entry", []int32{-1, 2, 1, 1, 1}, 4, false},
	}
	for _, c := range cases {
		if _, ok := starsFromCounts(c.counts, c.count); ok != c.ok {
			t.Errorf("%s: ok = %v, want %v", c.name, ok, c.ok)
		}
	}
}

func withStarCounts(rv *fakeReviews, id string, counts ...int32) *fakeReviews {
	rv.summaries[id].StarCounts = counts
	return rv
}

func TestProduct_RatingBarsMarkup(t *testing.T) {
	// pan-01 has 12 reviews in the fake
	rv := withStarCounts(sampleReviews(), "pan-01", 1, 1, 2, 3, 5)
	fe, _ := frontendWithReviews(t, rv)
	rr := do(fe, http.MethodGet, "/product/pan-01", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	html := rr.Body.String()
	assertContains(t, html, `class="rating-bars"`,
		// a text alternative per bar, in order 5 stars first
		`5 reviews rated 5 stars`, `3 reviews rated 4 stars`, `2 reviews rated 3 stars`, `1 review rated 2 stars`, `1 review rated 1 star`,
		`style="width: 41.7%"`, `style="width: 25%"`, `rating-bar-count" aria-hidden="true">5<`)
	if strings.Index(html, "5 reviews rated 5 stars") > strings.Index(html, "1 review rated 1 star") {
		t.Error("5 stars must be the top row")
	}
	if n := strings.Count(html, `class="rating-bar"`); n != 5 {
		t.Errorf("want 5 rows, got %d", n)
	}
	if len(rv.summaryCalls) != 1 || !reflect.DeepEqual(rv.summaryCalls[0], []string{"pan-01"}) {
		t.Errorf("summary calls: %v", rv.summaryCalls)
	}
}

func TestProduct_RatingBarsHiddenWhenNotAvailable(t *testing.T) {
	bad := sampleReviews()
	bad.summaries["pan-01"].StarCounts = []int32{12, 0, 0, 0, 1} // adds up to 13, not 12
	cases := map[string]struct {
		rv     *fakeReviews
		target string
	}{
		"older review-service without star_counts": {sampleReviews(), "/product/pan-01"},
		"distribution disagrees with the count":    {bad, "/product/pan-01"},
		"no reviews at all":                        {withStarCounts(sampleReviews(), "pan-02", 0, 0, 0, 0, 0), "/product/pan-03"},
	}
	for name, c := range cases {
		fe, _ := frontendWithReviews(t, c.rv)
		rr := do(fe, http.MethodGet, c.target, "", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: status %d", name, rr.Code)
		}
		if strings.Contains(rr.Body.String(), "rating-bar") {
			t.Errorf("%s: no bars expected", name)
		}
	}
	// the page still shows the reviews when there are no bars
	fe, _ := frontendWithReviews(t, sampleReviews())
	assertContains(t, do(fe, http.MethodGet, "/product/pan-01", "", nil).Body.String(), "Customer reviews", "Reviewer 1", "12 reviews")
}

// summaryFails answers ListReviews but not GetRatingSummary.
type summaryFails struct{ *fakeReviews }

func (summaryFails) GetRatingSummary(context.Context, *pb.GetRatingSummaryRequest, ...grpc.CallOption) (*pb.GetRatingSummaryResponse, error) {
	return nil, status.Error(codes.Unavailable, "down")
}

func TestProduct_SummaryFailureOnlyDropsTheBars(t *testing.T) {
	fe, _ := frontendWithReviews(t, nil)
	fe.reviews = summaryFails{withStarCounts(sampleReviews(), "pan-01", 1, 1, 2, 3, 5)}
	rr := do(fe, http.MethodGet, "/product/pan-01", "", nil)
	html := rr.Body.String()
	if rr.Code != http.StatusOK || !strings.Contains(html, "Reviewer 1") || strings.Contains(html, "rating-bar") {
		t.Errorf("status %d, reviews shown: %v, bars: %v", rr.Code, strings.Contains(html, "Reviewer 1"), strings.Contains(html, "rating-bar"))
	}
}

func TestReviewsFragmentMakesNoSummaryCall(t *testing.T) {
	rv := withStarCounts(sampleReviews(), "pan-01", 1, 1, 2, 3, 5)
	fe, _ := frontendWithReviews(t, rv)
	rr := do(fe, http.MethodGet, "/product/pan-01?rpage=2", "", map[string]string{"HX-Request": "true"})
	if rr.Code != http.StatusOK || len(rv.summaryCalls) != 0 || strings.Contains(rr.Body.String(), "rating-bar") {
		t.Errorf("status %d, summary calls %d", rr.Code, len(rv.summaryCalls))
	}
}

func TestGrid_StarCountsDoNotChangeTheCards(t *testing.T) {
	rv := withStarCounts(sampleReviews(), "pan-01", 1, 1, 2, 3, 5)
	fe, _ := frontendWithReviews(t, rv)
	html := do(fe, http.MethodGet, "/", "", nil).Body.String()
	assertContains(t, html, `aria-label="4.5 out of 5, 12 reviews"`)
	if strings.Contains(html, "rating-bar") {
		t.Error("cards show no bars")
	}
}

// ---------------------------------------------------------------------------
// ads
// ---------------------------------------------------------------------------

// adRecorder is an ad-service that records what the frontend asks.
type adRecorder struct {
	pb.UnimplementedAdServiceServer
	mu       sync.Mutex
	adsReq   [][]string
	clicks   []string
	ads      []*pb.Ad
	getErr   error
	clickErr error
	hang     bool // RecordClick waits for the caller's deadline
}

func (a *adRecorder) GetAds(_ context.Context, r *pb.AdRequest) (*pb.AdResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.adsReq = append(a.adsReq, append([]string{}, r.ContextKeys...))
	if a.getErr != nil {
		return nil, a.getErr
	}
	return &pb.AdResponse{Ads: a.ads}, nil
}

func (a *adRecorder) RecordClick(ctx context.Context, r *pb.RecordClickRequest) (*pb.RecordClickResponse, error) {
	a.mu.Lock()
	a.clicks = append(a.clicks, r.AdId)
	hang, err := a.hang, a.clickErr
	a.mu.Unlock()
	if hang {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	return &pb.RecordClickResponse{}, nil
}

func (a *adRecorder) keys() [][]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([][]string(nil), a.adsReq...)
}

func (a *adRecorder) clickIDs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.clicks...)
}

func frontendWithAds(t *testing.T, ads *adRecorder) *frontendServer {
	t.Helper()
	fe := newFakeFrontend(t, sampleCatalog())
	fe.adSvcConn = connTo(t, func(s *grpc.Server) { pb.RegisterAdServiceServer(s, ads) })
	return fe
}

var womenAd = &pb.Ad{Id: "women-1", Text: "Spring wardrobe", RedirectUrl: "/?category=women&sort=rating"}

func TestAdContextKeys_PerPageType(t *testing.T) {
	cases := []struct {
		name, target string
		want         []string
	}{
		{"landing page: no keys", "/", []string{}},
		{"department page: the department id", "/?category=kitchen", []string{"kitchen"}},
		{"department id is lower-cased", "/?category=KITCHEN", []string{"kitchen"}},
		{"search page: lower-cased words", "/?q=Cast+IRON++Pan", []string{"cast", "iron", "pan"}},
		{"search words are capped at 5", "/?q=a+b+c+d+e+f+g", []string{"a", "b", "c", "d", "e"}},
		{"department and search", "/?category=kitchen&q=Pan", []string{"kitchen", "pan"}},
		{"paging does not matter", "/?category=kitchen&page=2&sort=price_asc", []string{"kitchen"}},
		{"product page: the product's categories as before", "/product/pan-05", []string{"kitchen-accessories"}},
	}
	for _, c := range cases {
		ads := &adRecorder{ads: []*pb.Ad{womenAd}}
		fe := frontendWithAds(t, ads)
		if rr := do(fe, http.MethodGet, c.target, "", nil); rr.Code != http.StatusOK {
			t.Fatalf("%s: status %d", c.name, rr.Code)
		}
		got := ads.keys()
		if len(got) != 1 || !reflect.DeepEqual(got[0], c.want) {
			t.Errorf("%s: context_keys %v, want [%v]", c.name, got, c.want)
		}
	}
}

func TestAdContextKeys_Unit(t *testing.T) {
	if got := searchWords("  Ünï  CODE\tX "); !reflect.DeepEqual(got, []string{"ünï", "code", "x"}) {
		t.Errorf("searchWords: %v", got)
	}
	if got := searchWords("   "); len(got) != 0 {
		t.Errorf("blank search: %v", got)
	}
	if got := adContextKeys(browseParams{}); got == nil || len(got) != 0 {
		t.Errorf("no params must give an empty, non-nil list: %#v", got)
	}
}

func TestAd_LinkGoesThroughTheClickCounter(t *testing.T) {
	ads := &adRecorder{ads: []*pb.Ad{womenAd}}
	fe := frontendWithAds(t, ads)
	html := do(fe, http.MethodGet, "/", "", nil).Body.String()
	want := `href="/ads/women-1/click?to=%2F%3Fcategory%3Dwomen%26sort%3Drating"`
	assertContains(t, html, want, "Spring wardrobe", `rel="nofollow noopener noreferrer"`)
	if strings.Contains(html, `href="/?category=women&amp;sort=rating"`) {
		t.Error("the ad must not link straight to its target")
	}
}

func TestAd_WithoutIdKeepsTheDirectLink(t *testing.T) {
	// an ad-service from before click tracking sends no id
	ads := &adRecorder{ads: []*pb.Ad{{Text: "Old ad", RedirectUrl: "/?category=kitchen"}}}
	html := do(frontendWithAds(t, ads), http.MethodGet, "/", "", nil).Body.String()
	assertContains(t, html, `href="/?category=kitchen"`, "Old ad")
	if strings.Contains(html, "/ads/") {
		t.Error("no click link without an id")
	}
}

func TestAd_DownOrEmptyLeavesThePageUnchanged(t *testing.T) {
	for name, ads := range map[string]*adRecorder{
		"ad-service down": {getErr: status.Error(codes.Unavailable, "down")},
		"no ads":          {},
	} {
		fe := frontendWithAds(t, ads)
		for _, target := range []string{"/", "/?category=kitchen", "/product/pan-05"} {
			rr := do(fe, http.MethodGet, target, "", nil)
			if rr.Code != http.StatusOK || strings.Contains(rr.Body.String(), `class="ad-slot`) {
				t.Errorf("%s %s: status %d, ad slot shown: %v", name, target, rr.Code, strings.Contains(rr.Body.String(), `class="ad-slot`))
			}
		}
	}
}

// ---------------------------------------------------------------------------
// click redirect
// ---------------------------------------------------------------------------

func click(fe *frontendServer, id, to string) *httpResult {
	target := "/ads/" + id + "/click"
	if to != "\x00none" {
		target += "?to=" + url.QueryEscape(to)
	}
	start := time.Now()
	rr := do(fe, http.MethodGet, target, "", nil)
	return &httpResult{code: rr.Code, loc: rr.Header().Get("Location"), took: time.Since(start), cache: rr.Header().Get("Cache-Control")}
}

type httpResult struct {
	code       int
	loc, cache string
	took       time.Duration
}

func TestClick_LocalPathsAreFollowedAndCounted(t *testing.T) {
	for _, to := range []string{"/", "/?category=women", "/?category=women&sort=rating", "/product/pan-05", "/cart", "/?q=caf%C3%A9", "/a:b"} {
		ads := &adRecorder{}
		res := click(frontendWithAds(t, ads), "women-1", to)
		if res.code != http.StatusFound || res.loc != to {
			t.Errorf("to=%q: status %d, Location %q", to, res.code, res.loc)
		}
		if got := ads.clickIDs(); !reflect.DeepEqual(got, []string{"women-1"}) {
			t.Errorf("to=%q: RecordClick calls %v", to, got)
		}
		if res.cache != "no-store" {
			t.Errorf("Cache-Control %q", res.cache)
		}
	}
}

func TestClick_OpenRedirectAttemptsGoHome(t *testing.T) {
	attempts := map[string]string{
		"protocol-relative":       "//evil.com",
		"protocol-relative path":  "//evil.com/x",
		"backslash":               `/\evil.com`,
		"backslash path":          `/\evil.com/x`,
		"absolute https":          "https://evil.com",
		"absolute http":           "http://evil.com/",
		"javascript":              "javascript:alert(1)",
		"JavaScript mixed case":   "JaVaScRiPt:alert(1)",
		"data":                    "data:text/html,<script>1</script>",
		"no leading slash":        "evil.com",
		"relative":                "../evil",
		"tab inside":              "/\t/evil.com",
		"newline inside":          "/\n/evil.com",
		"carriage return":         "/\r\nSet-Cookie: a=b",
		"NUL":                     "/a\x00b",
		"DEL":                     "/a\x7fb",
		"C1 control (NEL)":        "/a\u0085b",
		"leading tab":             "\t//evil.com",
		"leading space":           " //evil.com",
		"empty":                   "",
		"invalid UTF-8":           "/a\xffb",
		"very long":               "/" + strings.Repeat("a", maxRedirectLen),
		"scheme-like after slash": "/http://evil.com@",
		"userinfo form":           "//user@evil.com",
	}
	// "/http://evil.com@" is a harmless local path: the whole string is path and query of this site
	delete(attempts, "scheme-like after slash")
	for name, to := range attempts {
		ads := &adRecorder{}
		res := click(frontendWithAds(t, ads), "women-1", to)
		if res.code != http.StatusFound || res.loc != "/" {
			t.Errorf("%s (%q): status %d, Location %q, want 302 to /", name, to, res.code, res.loc)
		}
		if len(ads.clickIDs()) != 1 {
			t.Errorf("%s: the click is still counted", name)
		}
	}
	// no `to` at all
	res := click(frontendWithAds(t, &adRecorder{}), "women-1", "\x00none")
	if res.code != http.StatusFound || res.loc != "/" {
		t.Errorf("missing to: %d %q", res.code, res.loc)
	}
}

func TestIsLocalPath(t *testing.T) {
	ok := []string{"/", "/x", "/?a=b", "/a/b?c=d#e", "/a%2F%2Fb", "/é", "/a\\b", "/a//b", "/a:b"}
	bad := []string{"", "//", "//x", `/\`, `/\x`, "x", "?a=b", "#a", "http://x", "https://x/", "javascript:1", "/\t", "/\n", "/\x00", "/\x1f", "/\x7f", "/\u0085", "/\xff", " /", "/%"}
	for _, s := range ok {
		if !isLocalPath(s) {
			t.Errorf("isLocalPath(%q) = false", s)
		}
	}
	for _, s := range bad {
		if isLocalPath(s) {
			t.Errorf("isLocalPath(%q) = true", s)
		}
	}
}

func TestClick_RecordClickFailureIsSilent(t *testing.T) {
	for name, ads := range map[string]*adRecorder{
		"unknown id":    {clickErr: status.Error(codes.NotFound, "no such ad")},
		"service error": {clickErr: status.Error(codes.Internal, "boom")},
		"unimplemented": {clickErr: status.Error(codes.Unimplemented, "old ad-service")},
		"unavailable":   {clickErr: status.Error(codes.Unavailable, "down")},
	} {
		res := click(frontendWithAds(t, ads), "women-1", "/?category=women")
		if res.code != http.StatusFound || res.loc != "/?category=women" {
			t.Errorf("%s: status %d, Location %q", name, res.code, res.loc)
		}
	}
}

func TestClick_HungAdServiceDelaysAtMostTheDeadline(t *testing.T) {
	old := adClickTimeout
	adClickTimeout = 150 * time.Millisecond
	defer func() { adClickTimeout = old }()
	res := click(frontendWithAds(t, &adRecorder{hang: true}), "women-1", "/?category=women")
	if res.code != http.StatusFound || res.loc != "/?category=women" {
		t.Errorf("status %d, Location %q", res.code, res.loc)
	}
	if res.took < 100*time.Millisecond || res.took > 2*time.Second {
		t.Errorf("took %v, want about the 150ms deadline", res.took)
	}
	if adClickTimeout != 150*time.Millisecond || old != time.Second {
		t.Errorf("the production deadline must be 1 s, was %v", old)
	}
}

func TestClick_AdServiceNeverConfiguredOrBadIdStillRedirects(t *testing.T) {
	fe := newFakeFrontend(t, sampleCatalog())
	fe.adSvcConn = nil
	if res := click(fe, "women-1", "/cart"); res.code != http.StatusFound || res.loc != "/cart" {
		t.Errorf("no ad connection: %d %q", res.code, res.loc)
	}
	for _, id := range []string{strings.Repeat("a", maxAdIDLen+1), "a b", "a%00b", "a;b", "é"} {
		ads := &adRecorder{}
		res := click(frontendWithAds(t, ads), url.PathEscape(id), "/cart")
		if res.code != http.StatusFound || res.loc != "/cart" {
			t.Errorf("id %q: %d %q", id, res.code, res.loc)
		}
		if got := ads.clickIDs(); len(got) != 0 {
			t.Errorf("id %q must not reach the ad-service, got %v", id, got)
		}
	}
}

func TestClick_OnlyGET(t *testing.T) {
	ads := &adRecorder{}
	fe := frontendWithAds(t, ads)
	for _, m := range []string{http.MethodPost, http.MethodHead, http.MethodPut} {
		if rr := do(fe, m, "/ads/women-1/click?to=%2F", "", nil); rr.Code == http.StatusFound || len(ads.clickIDs()) != 0 {
			t.Errorf("%s: status %d, clicks %v", m, rr.Code, ads.clickIDs())
		}
	}
}

// ---------------------------------------------------------------------------
// recommendations: what the pages pass
// ---------------------------------------------------------------------------

type recoRecorder struct {
	pb.UnimplementedRecommendationServiceServer
	mu   sync.Mutex
	reqs []*pb.ListRecommendationsRequest
}

func (r *recoRecorder) ListRecommendations(_ context.Context, in *pb.ListRecommendationsRequest) (*pb.ListRecommendationsResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, in)
	return &pb.ListRecommendationsResponse{ProductIds: []string{"pan-09"}}, nil
}

func frontendWithReco(t *testing.T, cart *fakeCart) (*frontendServer, *recoRecorder) {
	t.Helper()
	reco := &recoRecorder{}
	fe := newFakeFrontendWith(t, sampleCatalog(), cart, &fakeCheckout{})
	fe.recommendationSvcConn = connTo(t, func(s *grpc.Server) { pb.RegisterRecommendationServiceServer(s, reco) })
	return fe, reco
}

func TestRecommendations_ProductPagePassesTheViewedProduct(t *testing.T) {
	fe, reco := frontendWithReco(t, &fakeCart{items: []*pb.CartItem{{ProductId: "pan-07", Quantity: 1}}})
	rr := do(fe, http.MethodGet, "/product/pan-05", "", nil)
	if rr.Code != http.StatusOK || len(reco.reqs) != 1 {
		t.Fatalf("status %d, %d calls", rr.Code, len(reco.reqs))
	}
	if got := reco.reqs[0].ProductIds; !reflect.DeepEqual(got, []string{"pan-05"}) {
		t.Errorf("product page passes %v, want [pan-05]", got)
	}
	if reco.reqs[0].UserId != "test-session-id" {
		t.Errorf("user id %q", reco.reqs[0].UserId)
	}
	assertContains(t, rr.Body.String(), "Pan 09") // the answer is rendered
}

func TestRecommendations_CartPagePassesTheCartProducts(t *testing.T) {
	cart := &fakeCart{items: []*pb.CartItem{{ProductId: "pan-05", Quantity: 2}, {ProductId: "soap-1", Quantity: 1}, {ProductId: "OLJCESPC7Z", Quantity: 1}}}
	fe, reco := frontendWithReco(t, cart)
	rr := do(fe, http.MethodGet, "/cart", "", nil)
	if rr.Code != http.StatusOK || len(reco.reqs) != 1 {
		t.Fatalf("status %d, %d calls", rr.Code, len(reco.reqs))
	}
	// the products still in the catalogue, not the stale line
	if got := reco.reqs[0].ProductIds; !reflect.DeepEqual(got, []string{"pan-05", "soap-1"}) {
		t.Errorf("cart page passes %v, want [pan-05 soap-1]", got)
	}
}
