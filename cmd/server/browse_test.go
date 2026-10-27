package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	pb "frontend/genproto"
)

// ---------------------------------------------------------------------------
// fake backends (in-process gRPC over bufconn)
// ---------------------------------------------------------------------------

type fakeCatalog struct {
	pb.UnimplementedProductCatalogServiceServer
	products []*pb.Product
	cats     []*pb.Category
	lastReq  *pb.ListProductsRequest
}

func (f *fakeCatalog) ListCategories(context.Context, *pb.Empty) (*pb.ListCategoriesResponse, error) {
	return &pb.ListCategoriesResponse{Categories: f.cats}, nil
}

func (f *fakeCatalog) GetProduct(_ context.Context, r *pb.GetProductRequest) (*pb.Product, error) {
	for _, p := range f.products {
		if p.Id == r.Id {
			return p, nil
		}
	}
	return nil, fmt.Errorf("no product %q", r.Id)
}

// ListProductsFiltered mimics the catalog: filter, sort by name, page (page past the end = empty).
func (f *fakeCatalog) ListProductsFiltered(_ context.Context, r *pb.ListProductsRequest) (*pb.ListProductsResponse, error) {
	f.lastReq = r
	var out []*pb.Product
	for _, p := range f.products {
		if r.Department != "" && p.Department != r.Department {
			continue
		}
		if r.Query != "" && !strings.Contains(strings.ToLower(p.Name+" "+strings.Join(p.Tags, " ")), strings.ToLower(r.Query)) {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	page, size := int(r.Page), int(r.PageSize)
	if page < 1 {
		page = 1
	}
	total := len(out)
	lo, hi := (page-1)*size, page*size
	if lo > total {
		lo = total
	}
	if hi > total {
		hi = total
	}
	return &pb.ListProductsResponse{Products: out[lo:hi], Total: int32(total), Page: int32(page), PageSize: int32(size)}, nil // #nosec G115 -- test data
}

type fakeCurrency struct {
	pb.UnimplementedCurrencyServiceServer
}

func (fakeCurrency) GetSupportedCurrencies(context.Context, *pb.Empty) (*pb.GetSupportedCurrenciesResponse, error) {
	return &pb.GetSupportedCurrenciesResponse{CurrencyCodes: []string{"USD", "EUR"}}, nil
}
func (fakeCurrency) Convert(_ context.Context, r *pb.CurrencyConversionRequest) (*pb.Money, error) {
	return r.From, nil
}

type fakeCart struct {
	pb.UnimplementedCartServiceServer
}

func (fakeCart) GetCart(context.Context, *pb.GetCartRequest) (*pb.Cart, error) {
	return &pb.Cart{}, nil
}

type fakeReco struct {
	pb.UnimplementedRecommendationServiceServer
}

func (fakeReco) ListRecommendations(context.Context, *pb.ListRecommendationsRequest) (*pb.ListRecommendationsResponse, error) {
	return &pb.ListRecommendationsResponse{}, nil
}

type fakeAds struct {
	pb.UnimplementedAdServiceServer
}

func (fakeAds) GetAds(context.Context, *pb.AdRequest) (*pb.AdResponse, error) {
	return &pb.AdResponse{Ads: []*pb.Ad{{RedirectUrl: "/?category=kitchen", Text: "Kitchen week"}}}, nil
}

// newFakeFrontend starts one bufconn gRPC server with all fakes and points every connection at it.
func newFakeFrontend(t *testing.T, cat *fakeCatalog) *frontendServer {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	pb.RegisterProductCatalogServiceServer(srv, cat)
	pb.RegisterCurrencyServiceServer(srv, fakeCurrency{})
	pb.RegisterCartServiceServer(srv, fakeCart{})
	pb.RegisterRecommendationServiceServer(srv, fakeReco{})
	pb.RegisterAdServiceServer(srv, fakeAds{})
	go func() { _ = srv.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(); srv.Stop() })
	return &frontendServer{productCatalogSvcConn: conn, currencySvcConn: conn, cartSvcConn: conn,
		recommendationSvcConn: conn, adSvcConn: conn}
}

// sampleCatalog: 30 kitchen products (one out of stock, one low), 2 beauty products.
func sampleCatalog() *fakeCatalog {
	f := &fakeCatalog{cats: []*pb.Category{
		{Id: "kitchen", Name: "Kitchen", ProductCount: 30},
		{Id: "beauty", Name: "Beauty & care", ProductCount: 2},
	}}
	for i := 1; i <= 30; i++ {
		f.products = append(f.products, &pb.Product{
			Id: fmt.Sprintf("pan-%02d", i), Name: fmt.Sprintf("Pan %02d", i), Department: "kitchen",
			Categories: []string{"kitchen-accessories"}, Brand: "Panco", Rating: 4.5, Stock: 20,
			Thumbnail: fmt.Sprintf("/static/img/products/pan-%02d-thumb.webp", i),
			PriceUsd:  &pb.Money{CurrencyCode: "USD", Units: int64(i)},
		})
	}
	f.products[0].Stock = 0 // Pan 01
	f.products[1].Stock = 3 // Pan 02
	f.products = append(f.products,
		&pb.Product{Id: "soap-1", Name: "Soap <b>bar</b>", Department: "beauty", Tags: []string{"soap"}, Picture: "/static/img/products/soap-1-1.webp",
			Images: []string{"/static/img/products/soap-1-1.webp", "/static/img/products/soap-1-2.webp"}, Stock: 0, Rating: 3.2,
			PriceUsd: &pb.Money{CurrencyCode: "USD", Units: 4}},
		&pb.Product{Id: "cream-1", Name: "Cream", Department: "beauty", Tags: []string{"skin care"}, Stock: 4, PriceUsd: &pb.Money{CurrencyCode: "USD", Units: 9}},
	)
	return f
}

func serve(fe *frontendServer, target string, htmx bool) *httptest.ResponseRecorder {
	r := mux.NewRouter()
	r.HandleFunc("/", fe.homeHandler)
	r.HandleFunc("/product/{id}", fe.productHandler)
	req := requestWithContext(http.MethodGet, target, "")
	if htmx {
		req.Header.Set("HX-Request", "true")
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

// ---------------------------------------------------------------------------
// pure helpers
// ---------------------------------------------------------------------------

func TestParseBrowseParams(t *testing.T) {
	cases := []struct {
		query string
		want  browseParams
	}{
		{"", browseParams{Sort: "name", Page: 1}},
		{"category=Kitchen&q=+pan+&sort=price_desc&page=3", browseParams{Category: "kitchen", Query: "pan", Sort: "price_desc", Page: 3}},
		{"sort=bogus&page=-4", browseParams{Sort: "name", Page: 1}},
		{"page=abc", browseParams{Sort: "name", Page: 1}},
		{"page=99999999999", browseParams{Sort: "name", Page: 1}},
	}
	for _, c := range cases {
		got := parseBrowseParams(httptest.NewRequest(http.MethodGet, "/?"+c.query, nil))
		if got != c.want {
			t.Errorf("%q: got %+v, want %+v", c.query, got, c.want)
		}
	}
	long := parseBrowseParams(httptest.NewRequest(http.MethodGet, "/?q="+strings.Repeat("a", 500), nil))
	if len(long.Query) != maxQueryLen {
		t.Errorf("query not capped: %d", len(long.Query))
	}
}

func TestBrowseParamsURL_KeepsOtherParams(t *testing.T) {
	p := browseParams{Category: "kitchen", Query: "a&b", Sort: "rating", Page: 2}
	if got, want := p.url(3), "/?category=kitchen&page=3&q=a%26b&sort=rating"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := (browseParams{Sort: "name"}).url(1); got != "/" {
		t.Errorf("defaults should give /, got %q", got)
	}
}

func TestBuildPagination(t *testing.T) {
	if pg := buildPagination(browseParams{Sort: "name", Page: 1}, 24); pg.Pages != 1 || pg.Links != nil {
		t.Errorf("one page should have no links: %+v", pg)
	}
	pg := buildPagination(browseParams{Sort: "name", Page: 6}, 24*10)
	var got []string
	for _, l := range pg.Links {
		switch {
		case l.Gap:
			got = append(got, "...")
		case l.Current:
			got = append(got, fmt.Sprintf("[%d]", l.N))
		default:
			got = append(got, fmt.Sprint(l.N))
		}
	}
	if s := strings.Join(got, " "); s != "1 ... 4 5 [6] 7 8 ... 10" {
		t.Errorf("links: %s", s)
	}
	if pg.PrevURL != "/?page=5" || pg.NextURL != "/?page=7" {
		t.Errorf("prev/next: %q %q", pg.PrevURL, pg.NextURL)
	}
	last := buildPagination(browseParams{Sort: "name", Page: 10}, 24*10)
	if last.NextURL != "" || last.PrevURL == "" {
		t.Errorf("last page prev/next: %+v", last)
	}
}

func TestRatingHelpers(t *testing.T) {
	if ratingPct(4.3) != "86%" || ratingPct(7) != "100%" || ratingPct(-1) != "0%" {
		t.Errorf("ratingPct: %s %s %s", ratingPct(4.3), ratingPct(7), ratingPct(-1))
	}
	if fmtRating(4.25) != "4.2" && fmtRating(4.25) != "4.3" {
		t.Errorf("fmtRating: %s", fmtRating(4.25))
	}
}

// ---------------------------------------------------------------------------
// home handler with the fake catalog
// ---------------------------------------------------------------------------

func TestHome_FullPageFirstPage(t *testing.T) {
	cat := sampleCatalog()
	rr := serve(newFakeFrontend(t, cat), "/", false)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	html := rr.Body.String()
	assertContains(t, html, "<!DOCTYPE html>", `id="catalog-results"`, "32 products", "Shop by department",
		`href="/?category=beauty"`, "Beauty &amp; care", "2 products", // tiles with counts, escaped name
		`href="/product/cream-1"`, `href="/?page=2"`, "Next &rarr;", "Kitchen week",
		"Out of stock", "Only 3 left", "Only 4 left", "Panco",
		`src="/static/img/products/pan-03-thumb.webp"`)
	if cat.lastReq.PageSize != catalogPageSize || cat.lastReq.Page != 1 {
		t.Errorf("request: %+v", cat.lastReq)
	}
	if strings.Contains(html, "kitchen-accessories") {
		t.Error("card shows categories[0] instead of the department name")
	}
}

func TestHome_DepartmentFilterAndSearch(t *testing.T) {
	cat := sampleCatalog()
	rr := serve(newFakeFrontend(t, cat), "/?category=beauty&q=soap&sort=price_asc", false)
	html := rr.Body.String()
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	assertContains(t, html, "1 product", `href="/product/soap-1"`, "Soap &lt;b&gt;bar&lt;/b&gt;",
		`name="category" value="beauty"`, `value="soap"`, `<option value="price_asc" selected>`, `aria-current="page">Beauty &amp; care</a>`)
	if strings.Contains(html, "<b>bar</b>") {
		t.Error("product name not escaped")
	}
	if strings.Contains(html, "Shop by department") {
		t.Error("tiles should only show on the landing page")
	}
	if cat.lastReq.Department != "beauty" || cat.lastReq.Query != "soap" || cat.lastReq.Sort != pb.ProductSort_PRODUCT_SORT_PRICE_ASC {
		t.Errorf("request: %+v", cat.lastReq)
	}
}

func TestHome_UnknownCategoryIsEmptyState(t *testing.T) {
	cat := sampleCatalog()
	rr := serve(newFakeFrontend(t, cat), "/?category=cars", false)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	assertContains(t, rr.Body.String(), "Nothing found", "This department does not exist")
	if cat.lastReq != nil {
		t.Error("catalog should not be queried for an unknown department")
	}
}

func TestHome_NoSearchResults(t *testing.T) {
	rr := serve(newFakeFrontend(t, sampleCatalog()), "/?q=zzzz", false)
	assertContains(t, rr.Body.String(), "No products", "Nothing found", "No product matches your search")
	if strings.Contains(rr.Body.String(), `class="pagination"`) {
		t.Error("no pagination expected on an empty result")
	}
}

func TestHome_PageOutOfRangeFallsBackToLastPage(t *testing.T) {
	cat := sampleCatalog()
	rr := serve(newFakeFrontend(t, cat), "/?page=50", false)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	html := rr.Body.String()
	assertContains(t, html, "page 2 of 2", `aria-current="page">2</span>`, `href="/product/soap-1"`)
	if cat.lastReq.Page != 2 {
		t.Errorf("expected a retry with page 2, got %d", cat.lastReq.Page)
	}
}

func TestHome_PaginationKeepsParams(t *testing.T) {
	rr := serve(newFakeFrontend(t, sampleCatalog()), "/?category=kitchen&sort=rating", false)
	assertContains(t, rr.Body.String(), `href="/?category=kitchen&amp;page=2&amp;sort=rating"`, "30 products")
}

func TestHome_HTMXFragment(t *testing.T) {
	rr := serve(newFakeFrontend(t, sampleCatalog()), "/?page=2", true)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	html := strings.TrimSpace(rr.Body.String())
	if !strings.HasPrefix(html, `<div id="catalog-results"`) {
		t.Errorf("fragment should start with the results container, got %.80q", html)
	}
	for _, full := range []string{"<!DOCTYPE", "<header", "site-footer", "catalog-toolbar"} {
		if strings.Contains(html, full) {
			t.Errorf("fragment contains %q", full)
		}
	}
	assertContains(t, html, `href="/product/soap-1"`, "page 2 of 2")
	if rr.Header().Get("Vary") != "HX-Request" {
		t.Error("fragment response should Vary on HX-Request")
	}

	// a history restore must get the full page
	req := requestWithContext(http.MethodGet, "/?page=2", "")
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-History-Restore-Request", "true")
	if isFragmentRequest(req) {
		t.Error("history restore treated as fragment")
	}
}

// ---------------------------------------------------------------------------
// product page with the fake catalog
// ---------------------------------------------------------------------------

func TestProduct_GalleryBreadcrumbTagsOutOfStock(t *testing.T) {
	rr := serve(newFakeFrontend(t, sampleCatalog()), "/product/soap-1", false)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	html := rr.Body.String()
	assertContains(t, html,
		`<a href="/?category=beauty">Beauty &amp; care</a>`, // breadcrumb
		`href="/static/img/products/soap-1-2.webp"`,         // gallery thumbnail link (works without JS)
		`data-gallery-main`, `href="/?q=soap"`,
		`disabled>Out of stock</button>`, `name="product_id" value="soap-1"`)
	if strings.Contains(html, "Add to cart</button>") {
		t.Error("Add button should be replaced when stock is 0")
	}
	// no review client in these tests: no ratings, no reviews block (see reviews_test.go)
	if strings.Contains(html, "Customer reviews") || strings.Contains(html, `class="stars"`) {
		t.Error("reviews and stars should be hidden without a review service")
	}
}

func TestProduct_LowStockAndNoRating(t *testing.T) {
	html := serve(newFakeFrontend(t, sampleCatalog()), "/product/cream-1", false).Body.String()
	assertContains(t, html, "Only 4 left in stock", "Add to cart</button>", `href="/?q=skin%20care"`)
	if strings.Contains(html, `class="stars"`) {
		t.Error("stars should be hidden for rating 0")
	}
}

func TestProduct_InStock(t *testing.T) {
	html := serve(newFakeFrontend(t, sampleCatalog()), "/product/pan-05", false).Body.String()
	assertContains(t, html, "In stock", `<a href="/?category=kitchen">Kitchen</a>`, "Panco", "Add to cart</button>")
}
