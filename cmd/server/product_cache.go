package main

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	pb "frontend/genproto"
)

// Short in-process cache for the catalog lookups that repeat on every page: one GetProduct per cart
// line (the header cart count is on every page, the cart page lists the lines) and one per recommended
// id. The catalog has no batch RPC and none is added here (proto stays as it is), so the way to take
// the N+1 off the page is to ask once per TTL instead of once per request.
//
// Not used where freshness matters more than the RPC: the product page itself, add to cart and the
// review form read the catalog directly. Same rules as the currency cache: TTL, bounded, concurrent
// identical lookups share one RPC (singleflight), injectable clock, errors other than NotFound are
// never cached. NotFound is kept for the same TTL, otherwise a cart line whose product left the catalog
// would cost one RPC per request for the 48 h the cookie lives.
const (
	productCacheTTL        = 30 * time.Second
	productCacheMaxEntries = 1024
)

var productCacheTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "frontend_product_cache_total",
		Help: "Product lookups for cart lines and recommendations by result (hit, miss, shared = joined an identical call in flight)",
	},
	[]string{"op", "result"},
)

// configureForProducts sets the metric and the bound of the product cache (its metric and its bound). A zero
// ttlCache works too, with the defaults of the currency cache, which is what most tests use.
func (c *ttlCache) configureForProducts() {
	c.counter = productCacheTotal
	c.max = productCacheMaxEntries
}

// missingProduct is what the cache stores for a product the catalog answered NotFound for.
type missingProduct struct{ msg string }

// getProductCached is getProduct through the cache. The result is a copy: handlers may change it.
func (fe *frontendServer) getProductCached(ctx context.Context, id string) (*pb.Product, error) {
	v, err := fe.products.get(ctx, "product", id, productCacheTTL, func(ctx context.Context) (any, error) {
		p, err := fe.getProduct(ctx, id)
		if status.Code(err) == codes.NotFound {
			return missingProduct{msg: status.Convert(err).Message()}, nil
		}
		if err != nil {
			return nil, err
		}
		return p, nil
	})
	if err != nil {
		return nil, err
	}
	if m, ok := v.(missingProduct); ok {
		return nil, status.Error(codes.NotFound, m.msg)
	}
	return proto.Clone(v.(*pb.Product)).(*pb.Product), nil
}
