package main

import (
	"context"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	pb "frontend/genproto"
)

// Catalogue browsing on the home page: query parameters, paging and the view
// models used by templates/catalog.html.

const (
	catalogPageSize = 24
	maxQueryLen     = 100
	lowStockLimit   = 5
)

// sortOptions maps the `sort` query value to the catalog enum, in display order.
var sortOptions = []struct {
	Value, Label string
	Sort         pb.ProductSort
}{
	{"name", "Name A-Z", pb.ProductSort_PRODUCT_SORT_NAME_ASC},
	{"price_asc", "Price: low to high", pb.ProductSort_PRODUCT_SORT_PRICE_ASC},
	{"price_desc", "Price: high to low", pb.ProductSort_PRODUCT_SORT_PRICE_DESC},
	{"rating", "Top rated", pb.ProductSort_PRODUCT_SORT_RATING_DESC},
}

// browseParams are the validated query parameters of `/`.
type browseParams struct {
	Category string // department id, "" = all
	Query    string // search text, "" = none
	Sort     string // one of sortOptions[].Value
	Page     int    // 1-based
}

func parseBrowseParams(r *http.Request) browseParams {
	q := r.URL.Query()
	p := browseParams{
		Category: strings.ToLower(strings.TrimSpace(q.Get("category"))),
		Query:    strings.TrimSpace(q.Get("q")),
		Sort:     sortOptions[0].Value,
		Page:     1,
	}
	if len(p.Query) > maxQueryLen {
		p.Query = p.Query[:maxQueryLen]
	}
	for _, o := range sortOptions {
		if q.Get("sort") == o.Value {
			p.Sort = o.Value
		}
	}
	if n, err := strconv.Atoi(q.Get("page")); err == nil && n > 1 && n < 100000 {
		p.Page = n
	}
	return p
}

func (p browseParams) sortEnum() pb.ProductSort {
	for _, o := range sortOptions {
		if o.Value == p.Sort {
			return o.Sort
		}
	}
	return pb.ProductSort_PRODUCT_SORT_NAME_ASC
}

// url builds the `/` link for these params with the given page; defaults are left out.
func (p browseParams) url(page int) string {
	v := url.Values{}
	if p.Category != "" {
		v.Set("category", p.Category)
	}
	if p.Query != "" {
		v.Set("q", p.Query)
	}
	if p.Sort != sortOptions[0].Value {
		v.Set("sort", p.Sort)
	}
	if page > 1 {
		v.Set("page", strconv.Itoa(page))
	}
	if len(v) == 0 {
		return baseUrl + "/"
	}
	return baseUrl + "/?" + v.Encode()
}

// productCard is what a product card in the grid renders.
type productCard struct {
	Item     *pb.Product
	Price    *pb.Money
	Image    string // thumbnail, falls back to picture
	DeptName string
	Stock    stockInfo
	Rating   *ratingView // from review-service; nil = no reviews or service unavailable
}

type stockInfo struct {
	Out bool
	Low bool // 1..lowStockLimit left
	N   int32
}

func stockOf(p *pb.Product) stockInfo {
	n := p.GetStock()
	return stockInfo{Out: n <= 0, Low: n > 0 && n <= lowStockLimit, N: n}
}

func cardImage(p *pb.Product) string {
	if t := p.GetThumbnail(); t != "" {
		return t
	}
	return p.GetPicture()
}

type pageLink struct {
	N       int
	URL     string
	Current bool
	Gap     bool // an ellipsis instead of a link
}

type pagination struct {
	Page, Pages int
	PrevURL     string
	NextURL     string
	Links       []pageLink
}

// buildPagination returns numbered links (first, last, current +-2, gaps as ellipses).
func buildPagination(p browseParams, total int) pagination {
	pages := (total + catalogPageSize - 1) / catalogPageSize
	pg := pagination{Page: p.Page, Pages: pages}
	if pages <= 1 {
		return pg
	}
	if p.Page > 1 {
		pg.PrevURL = p.url(p.Page - 1)
	}
	if p.Page < pages {
		pg.NextURL = p.url(p.Page + 1)
	}
	last := 0
	for n := 1; n <= pages; n++ {
		if n != 1 && n != pages && (n < p.Page-2 || n > p.Page+2) {
			continue
		}
		if last != 0 && n > last+1 {
			pg.Links = append(pg.Links, pageLink{Gap: true})
		}
		pg.Links = append(pg.Links, pageLink{N: n, URL: p.url(n), Current: n == p.Page})
		last = n
	}
	return pg
}

func (fe *frontendServer) listCategories(ctx context.Context) ([]*pb.Category, error) {
	resp, err := pb.NewProductCatalogServiceClient(fe.productCatalogSvcConn).ListCategories(ctx, &pb.Empty{})
	return resp.GetCategories(), err
}

func (fe *frontendServer) listProductsFiltered(ctx context.Context, p browseParams) (*pb.ListProductsResponse, error) {
	return pb.NewProductCatalogServiceClient(fe.productCatalogSvcConn).ListProductsFiltered(ctx, &pb.ListProductsRequest{
		Department: p.Category,
		Query:      p.Query,
		Sort:       p.sortEnum(),
		Page:       int32(p.Page), // #nosec G115 -- bounded in parseBrowseParams
		PageSize:   catalogPageSize,
	})
}

// browse fetches one page of products for p. An unknown category gives an empty
// result without calling the catalog; a page past the end falls back to the last page.
func (fe *frontendServer) browse(ctx context.Context, p *browseParams, cats []*pb.Category) ([]*pb.Product, int, error) {
	if p.Category != "" && len(cats) > 0 && findCategory(cats, p.Category) == nil {
		return nil, 0, nil
	}
	resp, err := fe.listProductsFiltered(ctx, *p)
	if err != nil {
		return nil, 0, err
	}
	total := int(resp.GetTotal())
	if len(resp.GetProducts()) == 0 && total > 0 && p.Page > 1 {
		p.Page = (total + catalogPageSize - 1) / catalogPageSize
		if resp, err = fe.listProductsFiltered(ctx, *p); err != nil {
			return nil, 0, err
		}
		total = int(resp.GetTotal())
	}
	if len(resp.GetProducts()) == 0 {
		p.Page = 1
	}
	return resp.GetProducts(), total, nil
}

func findCategory(cats []*pb.Category, id string) *pb.Category {
	for _, c := range cats {
		if c.GetId() == id {
			return c
		}
	}
	return nil
}

func deptName(cats []*pb.Category, id string) string {
	if c := findCategory(cats, id); c != nil {
		return c.GetName()
	}
	return ""
}

// isFragmentRequest is true for htmx requests that swap only the results area.
// History restores (HX-History-Restore-Request) need the full page.
func isFragmentRequest(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-History-Restore-Request") != "true"
}

// productPage is what templates/product.html renders.
type productPage struct {
	Item     *pb.Product
	Price    *pb.Money
	Images   []string // gallery, first one is the main image; never empty when the product has a picture
	Category *pb.Category
	Stock    stockInfo
}

func productImages(p *pb.Product) []string {
	if imgs := p.GetImages(); len(imgs) > 0 {
		return imgs
	}
	if p.GetPicture() != "" {
		return []string{p.GetPicture()}
	}
	return nil
}

// cardData bundles the page data and one card for the "product_card" template.
func cardData(root map[string]interface{}, card productCard) map[string]interface{} {
	return map[string]interface{}{"root": root, "card": card}
}

// ratingPct is the width of the filled stars, e.g. "86%".
func ratingPct(r float64) string {
	r = math.Max(0, math.Min(5, r))
	return strconv.Itoa(int(math.Round(r*20))) + "%"
}

func fmtRating(r float64) string { return strconv.FormatFloat(r, 'f', 1, 64) }
