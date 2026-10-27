package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "frontend/genproto"
)

// ---------------------------------------------------------------------------
// fake review client (implements pb.ReviewServiceClient, no network)
// ---------------------------------------------------------------------------

type fakeReviews struct {
	mu           sync.Mutex
	summaries    map[string]*pb.RatingSummary // by product id
	reviews      map[string][]*pb.Review      // newest first
	summaryCalls [][]string
	listCalls    []*pb.ListReviewsRequest
	added        []*pb.AddReviewRequest
	err          error // returned by every call when set
	addErr       error // returned by AddReview when set
	block        bool  // wait for the caller's deadline, like a hung service
}

func (f *fakeReviews) wait(ctx context.Context) error {
	if f.block {
		<-ctx.Done()
		return status.FromContextError(ctx.Err()).Err()
	}
	return f.err
}

func (f *fakeReviews) GetRatingSummary(ctx context.Context, in *pb.GetRatingSummaryRequest, _ ...grpc.CallOption) (*pb.GetRatingSummaryResponse, error) {
	f.mu.Lock()
	f.summaryCalls = append(f.summaryCalls, in.ProductIds)
	f.mu.Unlock()
	if err := f.wait(ctx); err != nil {
		return nil, err
	}
	out := &pb.GetRatingSummaryResponse{}
	for _, id := range in.ProductIds {
		if s, ok := f.summaries[id]; ok {
			out.Summaries = append(out.Summaries, s)
		} else {
			out.Summaries = append(out.Summaries, &pb.RatingSummary{ProductId: id})
		}
	}
	return out, nil
}

func (f *fakeReviews) ListReviews(ctx context.Context, in *pb.ListReviewsRequest, _ ...grpc.CallOption) (*pb.ListReviewsResponse, error) {
	f.mu.Lock()
	f.listCalls = append(f.listCalls, in)
	f.mu.Unlock()
	if err := f.wait(ctx); err != nil {
		return nil, err
	}
	all := f.reviews[in.ProductId]
	page, size := int(in.Page), int(in.PageSize)
	if page < 1 {
		page = 1
	}
	lo, hi := min((page-1)*size, len(all)), min(page*size, len(all))
	resp := &pb.ListReviewsResponse{Reviews: all[lo:hi], Total: int32(len(all))} // #nosec G115 -- test data
	if s, ok := f.summaries[in.ProductId]; ok {
		resp.AverageRating = s.Average
	}
	return resp, nil
}

func (f *fakeReviews) AddReview(ctx context.Context, in *pb.AddReviewRequest, _ ...grpc.CallOption) (*pb.Review, error) {
	f.mu.Lock()
	f.added = append(f.added, in)
	f.mu.Unlock()
	if err := f.wait(ctx); err != nil {
		return nil, err
	}
	if f.addErr != nil {
		return nil, f.addErr
	}
	return &pb.Review{Id: "new", ProductId: in.ProductId, AuthorName: in.AuthorName, Rating: in.Rating, Comment: in.Comment}, nil
}

// reviewList makes n reviews, newest first, "Reviewer 1" is the newest.
func reviewList(productID string, n int) []*pb.Review {
	var out []*pb.Review
	for i := 1; i <= n; i++ {
		out = append(out, &pb.Review{
			Id: fmt.Sprintf("r%d", i), ProductId: productID, AuthorName: fmt.Sprintf("Reviewer %d", i),
			Rating: int32(1 + i%5), Comment: fmt.Sprintf("Comment number %d", i), // #nosec G115 -- test data
			CreatedAt: timestamppb.New(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC).AddDate(0, 0, -i)),
		})
	}
	return out
}

// sampleReviews: pan-01 has 12 reviews (3 pages of 5), pan-02 has 1, the rest none.
func sampleReviews() *fakeReviews {
	return &fakeReviews{
		summaries: map[string]*pb.RatingSummary{
			"pan-01": {ProductId: "pan-01", Average: 4.5, Count: 12},
			"pan-02": {ProductId: "pan-02", Average: 3, Count: 1},
		},
		reviews: map[string][]*pb.Review{"pan-01": reviewList("pan-01", 12), "pan-02": reviewList("pan-02", 1)},
	}
}

func frontendWithReviews(t *testing.T, rv *fakeReviews) (*frontendServer, *fakeCatalog) {
	t.Helper()
	cat := sampleCatalog()
	fe := newFakeFrontend(t, cat)
	if rv != nil {
		fe.reviews = rv
	}
	return fe, cat
}

// do routes a request through the same routes as main.go (without the middleware).
func do(fe *frontendServer, method, target, body string, header map[string]string) *httptest.ResponseRecorder {
	r := mux.NewRouter()
	r.HandleFunc("/", fe.homeHandler).Methods(http.MethodGet)
	r.HandleFunc("/product/{id}", fe.productHandler).Methods(http.MethodGet)
	r.HandleFunc("/product/{id}/reviews", fe.postReviewHandler).Methods(http.MethodPost)
	r.HandleFunc("/cart", fe.viewCartHandler).Methods(http.MethodGet)
	r.HandleFunc("/cart", fe.addToCartHandler).Methods(http.MethodPost)
	r.HandleFunc("/cart/empty", fe.emptyCartHandler).Methods(http.MethodPost)
	r.HandleFunc("/cart/checkout", fe.placeOrderHandler).Methods(http.MethodPost)
	req := requestWithContext(method, target, body)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

func form(kv ...string) string {
	v := url.Values{}
	for i := 0; i < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v.Encode()
}

// ---------------------------------------------------------------------------
// grid
// ---------------------------------------------------------------------------

func TestGrid_ShowsRatingsFromOneBatchedCall(t *testing.T) {
	rv := sampleReviews()
	fe, _ := frontendWithReviews(t, rv)
	rr := do(fe, http.MethodGet, "/", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	html := rr.Body.String()
	assertContains(t, html, `aria-label="4.5 out of 5, 12 reviews"`, `aria-label="3.0 out of 5, 1 review"`, "(12)", `width: 90%`)
	if n := strings.Count(html, `class="rating"`); n != 2 {
		t.Errorf("expected stars on the 2 reviewed products only, got %d", n)
	}
	if len(rv.summaryCalls) != 1 {
		t.Fatalf("expected ONE GetRatingSummary call, got %d", len(rv.summaryCalls))
	}
	if got := rv.summaryCalls[0]; len(got) != catalogPageSize || got[0] != "cream-1" {
		t.Errorf("batch should hold the %d ids of the page, got %d: %v", catalogPageSize, len(got), got)
	}
	if len(rv.listCalls) != 0 {
		t.Error("the grid must not list reviews")
	}
}

func TestGrid_FragmentSearchAndPagingAlsoGetRatings(t *testing.T) {
	for _, target := range []string{"/?page=2", "/?category=kitchen&sort=price_asc", "/?q=pan"} {
		rv := sampleReviews()
		fe, _ := frontendWithReviews(t, rv)
		rr := do(fe, http.MethodGet, target, "", map[string]string{"HX-Request": "true"})
		if rr.Code != http.StatusOK || len(rv.summaryCalls) != 1 {
			t.Errorf("%s (htmx): status %d, %d summary calls", target, rr.Code, len(rv.summaryCalls))
		}
		rv = sampleReviews()
		fe, _ = frontendWithReviews(t, rv)
		if rr := do(fe, http.MethodGet, target, "", nil); rr.Code != http.StatusOK || len(rv.summaryCalls) != 1 {
			t.Errorf("%s (full): status %d, %d summary calls", target, rr.Code, len(rv.summaryCalls))
		}
	}
	// the fragment of page 1 contains the stars of pan-01
	fe, _ := frontendWithReviews(t, sampleReviews())
	frag := do(fe, http.MethodGet, "/?q=pan", "", map[string]string{"HX-Request": "true"}).Body.String()
	assertContains(t, frag, `aria-label="4.5 out of 5, 12 reviews"`)
}

func TestGrid_NoCallForEmptyPage(t *testing.T) {
	rv := sampleReviews()
	fe, _ := frontendWithReviews(t, rv)
	if rr := do(fe, http.MethodGet, "/?q=zzzz", "", nil); rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	if len(rv.summaryCalls) != 0 {
		t.Errorf("no products, no ratings call; got %d", len(rv.summaryCalls))
	}
}

func TestPages_RenderWithoutStarsWhenReviewServiceIsDown(t *testing.T) {
	for name, rv := range map[string]*fakeReviews{
		"unavailable": {err: status.Error(codes.Unavailable, "connection refused")},
		"internal":    {err: status.Error(codes.Internal, "boom")},
	} {
		fe, _ := frontendWithReviews(t, rv)
		home := do(fe, http.MethodGet, "/", "", nil)
		if home.Code != http.StatusOK {
			t.Fatalf("%s: home status %d", name, home.Code)
		}
		assertContains(t, home.Body.String(), `class="product-card"`, `href="/product/pan-01"`)
		if strings.Contains(home.Body.String(), `class="stars"`) || strings.Contains(home.Body.String(), `class="rating"`) {
			t.Errorf("%s: no stars expected on the grid", name)
		}

		prod := do(fe, http.MethodGet, "/product/pan-01", "", nil)
		if prod.Code != http.StatusOK {
			t.Fatalf("%s: product status %d", name, prod.Code)
		}
		html := prod.Body.String()
		assertContains(t, html, "Reviews are not available right now", `id="review-form"`, `name="product_id" value="pan-01"`)
		if strings.Contains(html, `class="review-list"`) || strings.Contains(html, `class="stars"`) {
			t.Errorf("%s: no list or stars expected", name)
		}
	}
}

func TestPages_HungReviewServiceIsCutOffByTheTimeout(t *testing.T) {
	fe, _ := frontendWithReviews(t, &fakeReviews{block: true})
	start := time.Now()
	home := do(fe, http.MethodGet, "/", "", nil)
	prod := do(fe, http.MethodGet, "/product/pan-01", "", nil)
	if home.Code != http.StatusOK || prod.Code != http.StatusOK {
		t.Fatalf("status %d / %d", home.Code, prod.Code)
	}
	if d := time.Since(start); d > 3*reviewReadTimeout {
		t.Errorf("two pages took %v with a hung review service; timeout is %v each", d, reviewReadTimeout)
	}
}

func TestPages_ReviewsSwitchedOff(t *testing.T) {
	fe, _ := frontendWithReviews(t, nil) // no client
	home := do(fe, http.MethodGet, "/", "", nil)
	prod := do(fe, http.MethodGet, "/product/pan-01", "", nil)
	if home.Code != http.StatusOK || prod.Code != http.StatusOK {
		t.Fatalf("status %d / %d", home.Code, prod.Code)
	}
	for _, h := range []string{home.Body.String(), prod.Body.String()} {
		if strings.Contains(h, `class="stars"`) || strings.Contains(h, "Customer reviews") || strings.Contains(h, "review-form") {
			t.Error("nothing review-related expected when the service is switched off")
		}
	}
	if rr := do(fe, http.MethodPost, "/product/pan-01/reviews", form("author_name", "A", "rating", "5", "comment", "x"), nil); rr.Code != http.StatusNotFound {
		t.Errorf("post with reviews off: status %d, want 404", rr.Code)
	}
}

func TestReviewAddrFromEnv(t *testing.T) {
	t.Setenv("REVIEW_SERVICE_ADDR", "reviews.example:9000")
	if got := reviewAddrFromEnv(); got != "reviews.example:9000" {
		t.Errorf("got %q", got)
	}
	t.Setenv("REVIEW_SERVICE_ADDR", "")
	if got := reviewAddrFromEnv(); got != "" {
		t.Errorf("empty means off, got %q", got)
	}
}

func TestReviewAddrDefault(t *testing.T) {
	// t.Setenv registers the restore; Unsetenv then makes it really unset
	t.Setenv("REVIEW_SERVICE_ADDR", "x")
	if err := unsetenv("REVIEW_SERVICE_ADDR"); err != nil {
		t.Fatal(err)
	}
	if got := reviewAddrFromEnv(); got != defaultReviewAddr || defaultReviewAddr != "review-service:8084" {
		t.Errorf("got %q", got)
	}
}

func TestRatingViewLabel(t *testing.T) {
	if got := (ratingView{Average: 4.5, Count: 12}).Label(); got != "4.5 out of 5, 12 reviews" {
		t.Error(got)
	}
	if got := (ratingView{Average: 5, Count: 1}).Label(); got != "5.0 out of 5, 1 review" {
		t.Error(got)
	}
}

// ---------------------------------------------------------------------------
// product page: summary, list, paging
// ---------------------------------------------------------------------------

func TestProduct_SummaryListAndForm(t *testing.T) {
	rv := sampleReviews()
	fe, _ := frontendWithReviews(t, rv)
	rr := do(fe, http.MethodGet, "/product/pan-01", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	html := rr.Body.String()
	assertContains(t, html,
		`href="#reviews"`, "4.5 out of 5, 12 reviews", // rating link under the title
		`id="reviews"`, "Customer reviews", `aria-label="4.5 out of 5, 12 reviews"`, "12 reviews",
		"Reviewer 1", "Reviewer 5", "Comment number 1", `datetime="2026-09-29"`, "29 September 2026",
		`class="review-more"`, `href="/product/pan-01?rpage=2#reviews"`, `hx-get="/product/pan-01?rpage=2"`, `hx-target="closest li"`,
		// the form
		`action="/product/pan-01/reviews#review-form"`, `<label for="review-name">Your name</label>`, `name="author_name"`,
		`<legend>Your rating</legend>`, `type="radio" name="rating" id="rating-1" value="1"`, `id="rating-5" value="5"`,
		`>5 stars</span>`, `>1 star</span>`, `<label for="review-comment">Your review</label>`, "Post review")
	if strings.Contains(html, "Reviewer 6") {
		t.Error("only 5 reviews per page")
	}
	if strings.Contains(html, "Newer reviews") {
		t.Error("no newer link on page 1")
	}
	if len(rv.listCalls) != 1 || rv.listCalls[0].ProductId != "pan-01" || rv.listCalls[0].Page != 1 || rv.listCalls[0].PageSize != reviewsPageSize {
		t.Errorf("list calls: %+v", rv.listCalls)
	}
	if len(rv.summaryCalls) != 0 {
		t.Error("the product page needs no summary call, ListReviews carries total and average")
	}
}

func TestProduct_NoReviewsYet(t *testing.T) {
	fe, _ := frontendWithReviews(t, sampleReviews())
	html := do(fe, http.MethodGet, "/product/pan-03", "", nil).Body.String()
	assertContains(t, html, "No reviews yet", `id="review-form"`)
	if strings.Contains(html, `class="product-rating"`) || strings.Contains(html, `class="review-list"`) {
		t.Error("no rating line and no list without reviews")
	}
}

func TestProduct_SingularReview(t *testing.T) {
	fe, _ := frontendWithReviews(t, sampleReviews())
	html := do(fe, http.MethodGet, "/product/pan-02", "", nil).Body.String()
	assertContains(t, html, "3.0 out of 5, 1 review", ">1 review<")
	if strings.Contains(html, "Show more reviews") {
		t.Error("single page: no load more")
	}
}

func TestProduct_ReviewPaging_NoJSLinks(t *testing.T) {
	rv := sampleReviews()
	fe, _ := frontendWithReviews(t, rv)
	html := do(fe, http.MethodGet, "/product/pan-01?rpage=2", "", nil).Body.String()
	assertContains(t, html, "Reviewer 6", "Reviewer 10", `href="/product/pan-01#reviews">&larr; Newer reviews`, `href="/product/pan-01?rpage=3#reviews"`)
	if strings.Contains(html, "Reviewer 5<") || strings.Contains(html, "Reviewer 11") {
		t.Error("page 2 should hold reviews 6-10 only")
	}
	if got := rv.listCalls[0].Page; got != 2 {
		t.Errorf("page %d requested", got)
	}
	last := do(fe, http.MethodGet, "/product/pan-01?rpage=3", "", nil).Body.String()
	assertContains(t, last, "Reviewer 11", "Reviewer 12", `href="/product/pan-01?rpage=2#reviews">&larr; Newer reviews`)
	if strings.Contains(last, "Show more reviews") {
		t.Error("no load more on the last page")
	}
	// junk and out-of-range page numbers fall back to page 1 / an empty page, never an error
	for _, q := range []string{"rpage=abc", "rpage=-3", "rpage=0", "rpage=99"} {
		if rr := do(fe, http.MethodGet, "/product/pan-01?"+q, "", nil); rr.Code != http.StatusOK {
			t.Errorf("%s: status %d", q, rr.Code)
		}
	}
}

func TestProduct_ReviewPaging_HTMXFragment(t *testing.T) {
	rv := sampleReviews()
	fe, cat := frontendWithReviews(t, rv)
	rr := do(fe, http.MethodGet, "/product/pan-01?rpage=2", "", map[string]string{"HX-Request": "true"})
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	html := strings.TrimSpace(rr.Body.String())
	if !strings.HasPrefix(html, `<li class="review">`) {
		t.Errorf("fragment should start with a review item, got %.80q", html)
	}
	assertContains(t, html, "Reviewer 6", "Reviewer 10", `hx-get="/product/pan-01?rpage=3"`)
	for _, full := range []string{"<!DOCTYPE", "<header", "review-form", "<ol"} {
		if strings.Contains(html, full) {
			t.Errorf("fragment contains %q", full)
		}
	}
	if rr.Header().Get("Vary") != "HX-Request" {
		t.Error("fragment should Vary on HX-Request")
	}
	if cat.lastReq != nil {
		t.Error("the fragment must not touch the catalogue")
	}
	// the last page: no more link
	last := do(fe, http.MethodGet, "/product/pan-01?rpage=3", "", map[string]string{"HX-Request": "true"}).Body.String()
	if strings.Contains(last, "Show more reviews") || !strings.Contains(last, "Reviewer 12") {
		t.Errorf("last fragment: %q", last)
	}
	// a service failure inside the fragment is a note, not a 500
	fe2, _ := frontendWithReviews(t, &fakeReviews{err: status.Error(codes.Unavailable, "down")})
	down := do(fe2, http.MethodGet, "/product/pan-01?rpage=2", "", map[string]string{"HX-Request": "true"})
	if down.Code != http.StatusOK || !strings.Contains(down.Body.String(), "not available") {
		t.Errorf("down fragment: %d %q", down.Code, down.Body.String())
	}
}

func TestProduct_ReviewTextIsEscaped(t *testing.T) {
	rv := sampleReviews()
	rv.reviews["pan-01"][0].AuthorName = `<script>alert("a")</script>`
	rv.reviews["pan-01"][0].Comment = `<img src=x onerror=alert(1)> & "quotes" <b>bold</b>`
	fe, _ := frontendWithReviews(t, rv)
	html := do(fe, http.MethodGet, "/product/pan-01", "", nil).Body.String()
	for _, raw := range []string{`<script>alert("a")`, `<img src=x`, `<b>bold</b>`} {
		if strings.Contains(html, raw) {
			t.Errorf("raw HTML from review text in the page: %q", raw)
		}
	}
	assertContains(t, html, `&lt;script&gt;alert(&#34;a&#34;)&lt;/script&gt;`, `&lt;img src=x onerror=alert(1)&gt; &amp; &#34;quotes&#34; &lt;b&gt;bold&lt;/b&gt;`)
	// and the same in the fragment
	frag := do(fe, http.MethodGet, "/product/pan-01?rpage=1", "", map[string]string{"HX-Request": "true"}).Body.String()
	if strings.Contains(frag, "<img src=x") || strings.Contains(frag, "<script>") {
		t.Error("raw HTML in the fragment")
	}
}

// ---------------------------------------------------------------------------
// posting a review
// ---------------------------------------------------------------------------

func TestPostReview_SuccessRedirectsBack(t *testing.T) {
	rv := sampleReviews()
	fe, _ := frontendWithReviews(t, rv)
	rr := do(fe, http.MethodPost, "/product/pan-01/reviews",
		form("author_name", "  Alex  ", "rating", "4", "comment", "  Solid pan.\nHeats evenly.  "), nil)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	if loc := rr.Header().Get("Location"); loc != "/product/pan-01?reviewed=1#reviews" {
		t.Errorf("location %q", loc)
	}
	if len(rv.added) != 1 {
		t.Fatalf("%d AddReview calls", len(rv.added))
	}
	if a := rv.added[0]; a.ProductId != "pan-01" || a.AuthorName != "Alex" || a.Rating != 4 || a.Comment != "Solid pan.\nHeats evenly." {
		t.Errorf("AddReview request: %+v", a)
	}
	// the page it redirects to shows the confirmation
	html := do(fe, http.MethodGet, "/product/pan-01?reviewed=1", "", nil).Body.String()
	assertContains(t, html, `role="status"`, "Thank you, your review has been posted.")
	if strings.Contains(do(fe, http.MethodGet, "/product/pan-01", "", nil).Body.String(), "your review has been posted") {
		t.Error("confirmation only after the redirect")
	}
}

func TestPostReview_ValidationErrorsAreInline(t *testing.T) {
	long := strings.Repeat("x", maxReviewComment+1)
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"all empty", form(), []string{"Please enter your name.", "Please choose a rating from 1 to 5 stars.", "Please write a few words about the product."}},
		{"whitespace only", form("author_name", "   ", "rating", "5", "comment", " \n "), []string{"Please enter your name.", "Please write a few words"}},
		{"rating 0", form("author_name", "A", "rating", "0", "comment", "ok"), []string{"Please choose a rating"}},
		{"rating 6", form("author_name", "A", "rating", "6", "comment", "ok"), []string{"Please choose a rating"}},
		{"rating text", form("author_name", "A", "rating", "five", "comment", "ok"), []string{"Please choose a rating"}},
		{"name too long", form("author_name", strings.Repeat("n", 81), "rating", "5", "comment", "ok"), []string{"at most 80 characters"}},
		{"comment too long", form("author_name", "A", "rating", "5", "comment", long), []string{"at most 2000 characters"}},
		{"nul", form("author_name", "A\x00B", "rating", "5", "comment", "ok"), []string{"characters that are not allowed"}},
	}
	for _, tc := range cases {
		rv := sampleReviews()
		fe, _ := frontendWithReviews(t, rv)
		rr := do(fe, http.MethodPost, "/product/pan-01/reviews", tc.body, nil)
		if rr.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status %d, want 422", tc.name, rr.Code)
			continue
		}
		html := rr.Body.String()
		assertContains(t, html, `role="alert"`, "Your review was not sent.", `id="review-form"`, `name="product_id" value="pan-01"`) // full product page again
		for _, w := range tc.want {
			if !strings.Contains(html, w) {
				t.Errorf("%s: page is missing %q", tc.name, w)
			}
		}
		if len(rv.added) != 0 {
			t.Errorf("%s: invalid review reached the service", tc.name)
		}
	}
}

func TestPostReview_ErrorPageKeepsInputAndEscapesIt(t *testing.T) {
	fe, _ := frontendWithReviews(t, sampleReviews())
	rr := do(fe, http.MethodPost, "/product/pan-01/reviews",
		form("author_name", `"><script>alert(1)</script>`, "rating", "3", "comment", ""), nil)
	html := rr.Body.String()
	if strings.Contains(html, "<script>alert(1)") {
		t.Error("raw script from the name field")
	}
	assertContains(t, html, `value="&#34;&gt;&lt;script&gt;alert(1)&lt;/script&gt;"`, `id="rating-3" value="3" required checked`,
		`aria-invalid="true" aria-describedby="review-comment-error"`, `id="review-comment-error"`)
	if strings.Contains(html, `id="review-name-error"`) {
		t.Error("name is valid, no error for it")
	}
}

func TestPostReview_ReviewServiceErrors(t *testing.T) {
	good := form("author_name", "Alex", "rating", "5", "comment", "Great")
	rv := sampleReviews()
	rv.addErr = status.Error(codes.Unavailable, "database not ready")
	fe, _ := frontendWithReviews(t, rv)
	rr := do(fe, http.MethodPost, "/product/pan-01/reviews", good, nil)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rr.Code)
	}
	assertContains(t, rr.Body.String(), "We could not save your review right now", `value="Alex"`, ">Great</textarea>")
	if strings.Contains(rr.Body.String(), "database not ready") {
		t.Error("internal error text leaked into the page")
	}

	rv = sampleReviews()
	rv.addErr = status.Error(codes.InvalidArgument, "bad")
	fe, _ = frontendWithReviews(t, rv)
	if rr := do(fe, http.MethodPost, "/product/pan-01/reviews", good, nil); rr.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(rr.Body.String(), "did not accept this review") {
		t.Errorf("invalid argument: status %d", rr.Code)
	}

	// a hung service ends in 503 after the write timeout, not a hang
	fe, _ = frontendWithReviews(t, nil)
	fe.reviews = &slowAdd{fakeReviews: sampleReviews()}
	start := time.Now()
	if rr := do(fe, http.MethodPost, "/product/pan-01/reviews", good, nil); rr.Code != http.StatusServiceUnavailable {
		t.Errorf("hung AddReview: status %d", rr.Code)
	}
	if d := time.Since(start); d > reviewWriteTimeout+time.Second {
		t.Errorf("took %v", d)
	}
}

// slowAdd answers reads but never answers AddReview.
type slowAdd struct{ *fakeReviews }

func (s *slowAdd) AddReview(ctx context.Context, _ *pb.AddReviewRequest, _ ...grpc.CallOption) (*pb.Review, error) {
	<-ctx.Done()
	return nil, status.FromContextError(ctx.Err()).Err()
}

func TestPostReview_UnknownProductAndCrossSite(t *testing.T) {
	good := form("author_name", "Alex", "rating", "5", "comment", "Great")
	rv := sampleReviews()
	fe, _ := frontendWithReviews(t, rv)
	if rr := do(fe, http.MethodPost, "/product/no-such/reviews", good, nil); rr.Code == http.StatusSeeOther || rr.Code < 400 {
		t.Errorf("unknown product: status %d", rr.Code)
	}
	if rr := do(fe, http.MethodPost, "/product/pan-01/reviews", good, map[string]string{"Sec-Fetch-Site": "cross-site"}); rr.Code != http.StatusForbidden {
		t.Errorf("cross-site: status %d, want 403", rr.Code)
	}
	for _, site := range []string{"same-origin", "same-site", "none", ""} {
		if rr := do(fe, http.MethodPost, "/product/pan-01/reviews", good, map[string]string{"Sec-Fetch-Site": site}); rr.Code != http.StatusSeeOther {
			t.Errorf("Sec-Fetch-Site %q: status %d, want 303", site, rr.Code)
		}
	}
	if len(rv.added) != 4 {
		t.Errorf("%d reviews stored, want 4 (unknown product and cross-site refused)", len(rv.added))
	}
}

func TestPostReview_BodyIsLimited(t *testing.T) {
	fe, _ := frontendWithReviews(t, sampleReviews())
	body := form("author_name", "A", "rating", "5", "comment", strings.Repeat("y", 200<<10))
	if rr := do(fe, http.MethodPost, "/product/pan-01/reviews", body, nil); rr.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400", rr.Code)
	}
}

func unsetenv(k string) error { return os.Unsetenv(k) }
