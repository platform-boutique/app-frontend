package main

import (
	"context"
	"sync"
	"time"

	pb "frontend/genproto"
	"github.com/pkg/errors"
)

const (
	avoidNoopCurrencyConversionRPC = false

	// Deadlines per backend call. A hung backend must end in a 503 page well before the
	// server's 30 s WriteTimeout drops the connection without an answer. Not configurable
	// on purpose (no env), change them here.
	adTimeout       = 100 * time.Millisecond // an ad is optional: no ad rather than a slow page
	checkoutTimeout = 15 * time.Second       // PlaceOrder charges the card, ships and sends the e-mail
)

// backendTimeout is the deadline of every other call (catalog, currency, cart,
// recommendation, shipping). A variable only so tests can shorten it.
var backendTimeout = 3 * time.Second

// callCtx derives the context of one backend call; the caller must call cancel.
func callCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, backendTimeout)
}

func (fe *frontendServer) getProduct(ctx context.Context, id string) (*pb.Product, error) {
	ctx, cancel := callCtx(ctx)
	defer cancel()
	resp, err := pb.NewProductCatalogServiceClient(fe.productCatalogSvcConn).
		GetProduct(ctx, &pb.GetProductRequest{Id: id})
	return resp, err
}

func (fe *frontendServer) getCart(ctx context.Context, userID string) ([]*pb.CartItem, error) {
	ctx, cancel := callCtx(ctx)
	defer cancel()
	resp, err := pb.NewCartServiceClient(fe.cartSvcConn).GetCart(ctx, &pb.GetCartRequest{UserId: userID})
	return resp.GetItems(), err
}

func (fe *frontendServer) emptyCart(ctx context.Context, userID string) error {
	ctx, cancel := callCtx(ctx)
	defer cancel()
	_, err := pb.NewCartServiceClient(fe.cartSvcConn).EmptyCart(ctx, &pb.EmptyCartRequest{UserId: userID})
	return err
}

func (fe *frontendServer) insertCart(ctx context.Context, userID, productID string, quantity int32) error {
	ctx, cancel := callCtx(ctx)
	defer cancel()
	_, err := pb.NewCartServiceClient(fe.cartSvcConn).AddItem(ctx, &pb.AddItemRequest{
		UserId: userID,
		Item: &pb.CartItem{
			ProductId: productID,
			Quantity:  quantity},
	})
	return err
}

func (fe *frontendServer) getShippingQuote(ctx context.Context, items []*pb.CartItem, currency string) (*pb.Money, error) {
	quoteCtx, cancel := callCtx(ctx)
	defer cancel()
	quote, err := pb.NewShippingServiceClient(fe.shippingSvcConn).GetQuote(quoteCtx,
		&pb.GetQuoteRequest{
			Address: nil,
			Items:   items})
	if err != nil {
		return nil, err
	}
	localized, err := fe.convertCurrency(ctx, quote.GetCostUsd(), currency)
	return localized, errors.Wrap(err, "failed to convert currency for shipping cost")
}

func (fe *frontendServer) getRecommendations(ctx context.Context, userID string, productIDs []string) ([]*pb.Product, error) {
	recoCtx, cancel := callCtx(ctx)
	defer cancel()
	resp, err := pb.NewRecommendationServiceClient(fe.recommendationSvcConn).ListRecommendations(recoCtx,
		&pb.ListRecommendationsRequest{UserId: userID, ProductIds: productIDs})
	if err != nil {
		return nil, err
	}

	ids := resp.GetProductIds()
	if len(ids) > 4 {
		ids = ids[:4] // cap early so we don't fetch more than we'll show
	}

	type result struct {
		product *pb.Product
		err     error
	}

	results := make([]result, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			p, err := fe.getProduct(ctx, id)
			results[i] = result{p, err}
		}(i, id)
	}
	wg.Wait()

	out := make([]*pb.Product, len(ids))
	for i, r := range results {
		if r.err != nil {
			return nil, errors.Wrapf(r.err, "failed to get recommended product info (#%s)", ids[i])
		}
		out[i] = r.product
	}
	return out, nil
}

func (fe *frontendServer) getAd(ctx context.Context, ctxKeys []string) ([]*pb.Ad, error) {
	ctx, cancel := context.WithTimeout(ctx, adTimeout)
	defer cancel()

	resp, err := pb.NewAdServiceClient(fe.adSvcConn).GetAds(ctx, &pb.AdRequest{
		ContextKeys: ctxKeys,
	})
	return resp.GetAds(), errors.Wrap(err, "failed to get ads")
}
