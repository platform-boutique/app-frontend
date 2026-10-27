package main

import (
	"context"
	"sync"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "frontend/genproto"
)

// cartLine is a cart line whose product still exists in the catalog.
type cartLine struct {
	Item     *pb.Product
	Quantity int32
}

// liveCartLines looks up the product of every cart line. A cart outlives the
// catalog (cookie 48 h, cart-service keeps it): lines whose product is gone
// (NotFound) are skipped and logged once per call, any other error is returned.
// The page and the Empty-cart button keep working with stale lines in the cart.
func (fe *frontendServer) liveCartLines(ctx context.Context, log logrus.FieldLogger, cart []*pb.CartItem) (lines []cartLine, stale int, err error) {
	products := make([]*pb.Product, len(cart))
	errs := make([]error, len(cart))
	var wg sync.WaitGroup
	for i, item := range cart {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			products[i], errs[i] = fe.getProduct(ctx, id)
		}(i, item.GetProductId())
	}
	wg.Wait()

	var staleIDs []string
	for i, item := range cart {
		switch {
		case errs[i] == nil:
			lines = append(lines, cartLine{Item: products[i], Quantity: item.GetQuantity()})
		case status.Code(errs[i]) == codes.NotFound:
			staleIDs = append(staleIDs, item.GetProductId())
		default:
			return nil, 0, errors.Wrapf(errs[i], "could not retrieve product #%s", item.GetProductId())
		}
	}
	if len(staleIDs) > 0 {
		log.WithField("products", staleIDs).Warn("cart has products that are no longer in the catalog, skipping them")
	}
	return lines, len(staleIDs), nil
}

func linesSize(lines []cartLine) int {
	n := 0
	for _, l := range lines {
		n += int(l.Quantity)
	}
	return n
}

func linesItems(lines []cartLine) []*pb.CartItem {
	out := make([]*pb.CartItem, len(lines))
	for i, l := range lines {
		out[i] = &pb.CartItem{ProductId: l.Item.GetId(), Quantity: l.Quantity}
	}
	return out
}

// headerCartSize is the number in the header cart badge: the cart without stale
// lines. If the catalog cannot be asked it falls back to the raw count.
func (fe *frontendServer) headerCartSize(ctx context.Context, log logrus.FieldLogger, cart []*pb.CartItem) int {
	if len(cart) == 0 {
		return 0
	}
	lines, _, err := fe.liveCartLines(ctx, log, cart)
	if err != nil {
		log.WithField("error", err).Warn("could not check the cart against the catalog, showing the raw count")
		return cartSize(cart)
	}
	return linesSize(lines)
}
