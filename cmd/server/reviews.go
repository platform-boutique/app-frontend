package main

import (
	"context"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gorilla/mux"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "frontend/genproto"
)

// Ratings on the grid and reviews on the product page, backed by review-service.
// The service is optional: when it is disabled, down or slow the pages render
// without stars and without the reviews section, never with a 500.

const (
	defaultReviewAddr = "review-service:8084"

	// reads sit on the page's critical path, so they are cut short
	reviewReadTimeout  = 400 * time.Millisecond
	reviewWriteTimeout = 2 * time.Second

	reviewsPageSize = 5

	// same limits as review-service, so the form can explain them before a round trip
	maxReviewName    = 80
	maxReviewComment = 2000
)

// reviewAddrFromEnv returns the review-service address: REVIEW_SERVICE_ADDR if set,
// the default otherwise. A variable that is set but empty switches reviews off.
func reviewAddrFromEnv() string {
	if v, ok := os.LookupEnv("REVIEW_SERVICE_ADDR"); ok {
		return strings.TrimSpace(v)
	}
	return defaultReviewAddr
}

// ratingView is the rating of one product as the templates show it.
type ratingView struct {
	Average float64
	Count   int32
	// Stars[0] is the number of 1-star reviews ... Stars[4] the 5-star ones. Only
	// meaningful when HasStars: an older review-service sends no distribution.
	Stars    [5]int32
	HasStars bool
}

// starsFromCounts validates the distribution of a summary: exactly five entries, none
// negative, adding up to count. Anything else (no field from an older service, an
// inconsistent answer) is not shown rather than guessed at.
func starsFromCounts(counts []int32, count int32) (stars [5]int32, ok bool) {
	if len(counts) != len(stars) || count <= 0 {
		return stars, false
	}
	var sum int64
	for i, c := range counts {
		if c < 0 {
			return stars, false
		}
		stars[i] = c
		sum += int64(c)
	}
	return stars, sum == int64(count)
}

// ratingBar is one row of the rating distribution.
type ratingBar struct {
	Stars   int
	Count   int32
	Percent float64 // share of all reviews, 0-100, one decimal
	Label   string  // the text alternative, e.g. "3 reviews rated 5 stars"
}

// Width is the CSS width of the filled part of the bar.
func (b ratingBar) Width() string { return strconv.FormatFloat(b.Percent, 'f', -1, 64) + "%" }

// Bars are the five rows of the distribution, 5 stars first. Nil when there is no
// distribution or no review.
func (r ratingView) Bars() []ratingBar {
	if !r.HasStars || r.Count <= 0 {
		return nil
	}
	out := make([]ratingBar, 0, len(r.Stars))
	for k := len(r.Stars); k >= 1; k-- {
		c := r.Stars[k-1]
		noun := " reviews"
		if c == 1 {
			noun = " review"
		}
		star := " stars"
		if k == 1 {
			star = " star"
		}
		out = append(out, ratingBar{
			Stars: k, Count: c,
			Percent: math.Round(float64(c)/float64(r.Count)*1000) / 10,
			Label:   strconv.Itoa(int(c)) + noun + " rated " + strconv.Itoa(k) + star,
		})
	}
	return out
}

// Label is the text for screen readers, e.g. "4.5 out of 5, 12 reviews".
func (r ratingView) Label() string {
	n := strconv.Itoa(int(r.Count)) + " reviews"
	if r.Count == 1 {
		n = "1 review"
	}
	return fmtRating(r.Average) + " out of 5, " + n
}

// getRatingSummaries fetches the ratings of all ids in ONE call. Products without
// reviews are left out of the result; any failure gives a nil map (no stars).
func (fe *frontendServer) getRatingSummaries(ctx context.Context, log logrus.FieldLogger, ids []string) map[string]ratingView {
	if fe.reviews == nil || len(ids) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, reviewReadTimeout)
	defer cancel()
	resp, err := fe.reviews.GetRatingSummary(ctx, &pb.GetRatingSummaryRequest{ProductIds: ids})
	if err != nil {
		log.WithField("error", err).Warn("ratings unavailable, rendering without stars")
		return nil
	}
	out := make(map[string]ratingView, len(resp.GetSummaries()))
	for _, s := range resp.GetSummaries() {
		if s.GetCount() > 0 {
			rv := ratingView{Average: s.GetAverage(), Count: s.GetCount()}
			rv.Stars, rv.HasStars = starsFromCounts(s.GetStarCounts(), s.GetCount())
			out[s.GetProductId()] = rv
		}
	}
	return out
}

// reviewView is one review as rendered.
type reviewView struct {
	Author  string
	Rating  int32
	Comment string
	Date    string // "2 January 2026"
	ISODate string // for <time datetime>
}

// Stars is the rating as the float the star template takes.
func (v reviewView) Stars() float64 { return float64(v.Rating) }

func newReviewView(r *pb.Review) reviewView {
	v := reviewView{Author: r.GetAuthorName(), Rating: r.GetRating(), Comment: r.GetComment()}
	if r.GetCreatedAt() != nil {
		t := r.GetCreatedAt().AsTime().UTC()
		v.Date = t.Format("2 January 2006")
		v.ISODate = t.Format("2006-01-02")
	}
	return v
}

// reviewsSection is the data of the reviews block on the product page.
type reviewsSection struct {
	Enabled     bool // false: the frontend has no review client, the block is not shown
	Unavailable bool // the service did not answer: show a note instead of the list
	Rating      ratingView
	Reviews     []reviewView
	Page        int
	NewerURL    string // no-JS paging: the page before / after
	OlderURL    string
	MoreURL     string // htmx "load more" fragment URL (same as OlderURL)
	Form        reviewForm
	Posted      bool // redirected back after a successful post
}

// reviewForm is the state of the "write a review" form, with inline errors.
type reviewForm struct {
	Name, Comment string
	Rating        int
	Errors        map[string]string // field -> message; "form" for errors not tied to a field
}

func (f reviewForm) HasErrors() bool { return len(f.Errors) > 0 }

// validate trims the values and fills Errors using the same limits as review-service.
func (f *reviewForm) validate() {
	f.Name = strings.TrimSpace(f.Name)
	f.Comment = strings.TrimSpace(f.Comment)
	f.Errors = map[string]string{}
	switch n := utf8.RuneCountInString(f.Name); {
	case n == 0:
		f.Errors["name"] = "Please enter your name."
	case n > maxReviewName:
		f.Errors["name"] = "Your name can be at most " + strconv.Itoa(maxReviewName) + " characters."
	case strings.ContainsRune(f.Name, 0):
		f.Errors["name"] = "Your name contains characters that are not allowed."
	}
	if f.Rating < 1 || f.Rating > 5 {
		f.Errors["rating"] = "Please choose a rating from 1 to 5 stars."
	}
	switch n := utf8.RuneCountInString(f.Comment); {
	case n == 0:
		f.Errors["comment"] = "Please write a few words about the product."
	case n > maxReviewComment:
		f.Errors["comment"] = "Your review can be at most " + strconv.Itoa(maxReviewComment) + " characters."
	case strings.ContainsRune(f.Comment, 0):
		f.Errors["comment"] = "Your review contains characters that are not allowed."
	}
	if len(f.Errors) == 0 {
		f.Errors = nil
	}
}

// reviewPageParam reads ?rpage= (1-based, bounded like the catalogue page).
func reviewPageParam(r *http.Request) int {
	if n, err := strconv.Atoi(r.URL.Query().Get("rpage")); err == nil && n > 1 && n < 100000 {
		return n
	}
	return 1
}

func reviewsURL(id string, page int, fragment bool) string {
	u := baseUrl + "/product/" + url.PathEscape(id)
	if page > 1 {
		u += "?rpage=" + strconv.Itoa(page)
	}
	if !fragment {
		u += "#reviews"
	}
	return u
}

// loadReviews fetches one page of reviews and fills the paging links. A failure
// marks the section unavailable; it never fails the page. With withStars the
// distribution (GetRatingSummary, in parallel) is fetched too: the bars are shown only
// when it agrees with the review count, a failure just leaves them out.
func (fe *frontendServer) loadReviews(ctx context.Context, log logrus.FieldLogger, id string, page int, withStars bool) reviewsSection {
	sec := reviewsSection{Enabled: fe.reviews != nil, Page: page}
	if !sec.Enabled {
		return sec
	}
	ctx, cancel := context.WithTimeout(ctx, reviewReadTimeout)
	defer cancel()
	var summary *pb.RatingSummary
	done := make(chan struct{})
	go func() {
		defer close(done)
		if !withStars {
			return
		}
		resp, err := fe.reviews.GetRatingSummary(ctx, &pb.GetRatingSummaryRequest{ProductIds: []string{id}})
		if err != nil {
			log.WithField("error", err).Warn("rating distribution unavailable, rendering without bars")
			return
		}
		for _, s := range resp.GetSummaries() {
			if s.GetProductId() == id {
				summary = s
			}
		}
	}()
	resp, err := fe.reviews.ListReviews(ctx, &pb.ListReviewsRequest{ProductId: id, Page: int32(page), PageSize: reviewsPageSize}) // #nosec G115 -- bounded in reviewPageParam
	<-done
	if err != nil {
		log.WithField("error", err).Warn("reviews unavailable, rendering without them")
		sec.Unavailable = true
		return sec
	}
	sec.Rating = ratingView{Average: resp.GetAverageRating(), Count: resp.GetTotal()}
	if summary != nil && summary.GetCount() == sec.Rating.Count {
		sec.Rating.Stars, sec.Rating.HasStars = starsFromCounts(summary.GetStarCounts(), summary.GetCount())
	}
	for _, r := range resp.GetReviews() {
		sec.Reviews = append(sec.Reviews, newReviewView(r))
	}
	if page > 1 {
		sec.NewerURL = reviewsURL(id, page-1, false)
	}
	if int(resp.GetTotal()) > page*reviewsPageSize {
		sec.OlderURL = reviewsURL(id, page+1, false)
		sec.MoreURL = reviewsURL(id, page+1, true)
	}
	return sec
}

// reviewsFragment answers an htmx "load more" request with just the next
// <li> items and the next load-more link.
func (fe *frontendServer) reviewsFragment(w http.ResponseWriter, r *http.Request, id string) {
	log := r.Context().Value(ctxKeyLog{}).(logrus.FieldLogger)
	sec := fe.loadReviews(r.Context(), log, id, reviewPageParam(r), false)
	w.Header().Set("Vary", "HX-Request")
	data := injectCommonTemplateData(r, map[string]interface{}{"reviews": sec})
	if err := getTemplates().ExecuteTemplate(w, "review_items", data); err != nil {
		log.Error(err)
	}
}

// postReviewHandler validates the form, stores the review and redirects back to
// the product page (post/redirect/get). Invalid input re-renders the product page
// with the values and the errors; an unreachable review service does the same with 503.
//
// CSRF: like the other forms there is no token. The only state the site keeps per
// visitor is in SameSite=Strict cookies, and a review carries no identity. On top
// of that, browsers that send Sec-Fetch-Site are refused when the post comes from
// another site.
func (fe *frontendServer) postReviewHandler(w http.ResponseWriter, r *http.Request) {
	log := r.Context().Value(ctxKeyLog{}).(logrus.FieldLogger)
	id := mux.Vars(r)["id"]
	if fe.reviews == nil {
		fe.renderHTTPError(log, r, w, errors.New("reviews are not enabled"), http.StatusNotFound)
		return
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		fe.renderHTTPError(log, r, w, errors.New("cross-site form post refused"), http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		fe.renderHTTPError(log, r, w, errors.Wrap(err, "could not read the form"), http.StatusBadRequest)
		return
	}
	rating, _ := strconv.Atoi(r.PostFormValue("rating"))
	form := reviewForm{Name: r.PostFormValue("author_name"), Comment: r.PostFormValue("comment"), Rating: rating}
	form.validate()

	// the product must exist: reviews for made-up ids would never be shown
	if _, err := fe.getProduct(r.Context(), id); err != nil {
		code := http.StatusInternalServerError
		if status.Code(err) == codes.NotFound {
			code = http.StatusNotFound
		}
		fe.renderHTTPError(log, r, w, errors.Wrap(err, "could not retrieve product"), code)
		return
	}
	if form.HasErrors() {
		fe.renderProduct(w, r, id, &form, http.StatusUnprocessableEntity)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), reviewWriteTimeout)
	defer cancel()
	_, err := fe.reviews.AddReview(ctx, &pb.AddReviewRequest{
		ProductId: id, AuthorName: form.Name, Rating: int32(form.Rating), Comment: form.Comment, // #nosec G115 -- validated 1..5
	})
	if err != nil {
		log.WithField("error", err).WithField("product", id).Warn("could not store review")
		code := http.StatusServiceUnavailable
		form.Errors = map[string]string{"form": "We could not save your review right now. Please try again in a moment."}
		switch status.Code(err) {
		case codes.InvalidArgument:
			code = http.StatusUnprocessableEntity
			form.Errors["form"] = "The review service did not accept this review. Please check your input."
		case codes.AlreadyExists:
			code = http.StatusUnprocessableEntity
			form.Errors["form"] = "You already posted this review for this product."
		case codes.ResourceExhausted:
			code = http.StatusTooManyRequests
			form.Errors["form"] = "You are posting reviews too quickly. Please wait a minute and try again."
		}
		fe.renderProduct(w, r, id, &form, code)
		return
	}
	log.WithField("product", id).WithField("rating", form.Rating).Info("review added")
	http.Redirect(w, r, baseUrl+"/product/"+url.PathEscape(id)+"?reviewed=1#reviews", http.StatusSeeOther)
}
