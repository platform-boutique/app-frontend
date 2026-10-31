package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"os"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	pb "frontend/genproto"
	pbi "frontend/genproto/inventory"
)

// Live stock from inventory-service. The catalogue's `stock` is the seed value; the
// inventory knows what is still free (total minus held and sold). The service is
// optional: when it is off, down, slow or does not know a product the page uses the
// catalogue's stock, never a 500.

const (
	defaultInventoryAddr = "inventory-service:8087"

	// the stock sits on the page's critical path, so the call is cut short
	inventoryReadTimeout = time.Second

	// GetStock takes 1-100 ids
	maxStockIDs = 100
)

// inventoryAddrFromEnv returns the inventory-service address: INVENTORY_SERVICE_ADDR if
// set, the default otherwise. A variable that is set but empty switches live stock off.
func inventoryAddrFromEnv() string {
	if v, ok := os.LookupEnv("INVENTORY_SERVICE_ADDR"); ok {
		return strings.TrimSpace(v)
	}
	return defaultInventoryAddr
}

// liveStock is the live stock of the products one page shows, by product id. A nil
// map (service off or down) is valid: of() then answers with the catalogue's stock.
type liveStock map[string]int32

// of is the stock to show and enforce for p: the live value, else the catalogue's.
func (s liveStock) of(p *pb.Product) int32 {
	if n, ok := s[p.GetId()]; ok {
		return n
	}
	return p.GetStock()
}

// info is the badge data for p.
func (s liveStock) info(p *pb.Product) stockInfo { return stockInfoOf(s.of(p)) }

// overlayStock is THE helper that overlays live stock on the products of a page: one
// GetStock call with all distinct ids, 1 s deadline. Any failure gives a nil map
// (logged at warn), so callers always fall back to the catalogue's numbers.
func (fe *frontendServer) overlayStock(ctx context.Context, log logrus.FieldLogger, products ...*pb.Product) liveStock {
	if fe.inventory == nil {
		return nil
	}
	seen := make(map[string]bool, len(products))
	ids := make([]string, 0, len(products))
	for _, p := range products {
		if id := p.GetId(); id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	if len(ids) > maxStockIDs {
		ids = ids[:maxStockIDs] // the rest keeps the catalogue's number
	}
	ctx, cancel := context.WithTimeout(ctx, inventoryReadTimeout)
	defer cancel()
	resp, err := fe.inventory.GetStock(ctx, &pbi.GetStockRequest{ProductIds: ids})
	if err != nil {
		log.WithField("error", err).Warn("live stock unavailable, using the catalogue's stock")
		return nil
	}
	out := make(liveStock, len(resp.GetLevels()))
	for _, l := range resp.GetLevels() {
		out[l.GetProductId()] = max(l.GetAvailable(), 0)
	}
	return out
}

// Checkout idempotency key: random, generated when the cart page (the checkout form) is
// rendered, NOT per submit. A double submit, a reload of the POST or the back button
// therefore send the same key and checkout answers with the same order instead of a second one.

const (
	idempotencyKeyBytes = 16 // 128 bits, 22 characters
	maxIdempotencyKey   = 64 // checkout's limit
)

// newIdempotencyKey returns 22 URL-safe characters ([A-Za-z0-9_-], inside checkout's [A-Za-z0-9._-]) from crypto/rand.
// If the system's randomness fails (it practically never does) it returns "": the order is then placed without
// replay protection, as before, rather than with a guessable key.
func newIdempotencyKey() string {
	b := make([]byte, idempotencyKeyBytes)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// validIdempotencyKey: empty is fine (an old form, no replay protection), else 1-64 of [A-Za-z0-9._-].
func validIdempotencyKey(k string) bool {
	if len(k) > maxIdempotencyKey {
		return false
	}
	for _, c := range k {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}
