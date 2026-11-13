package main

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	pb "frontend/genproto"
)

// In-process cache for currency-service (owner decision: in-process caches, no Memcached).
//
// What is cached and why it is exact:
//   - GetSupportedCurrencies (filtered by the whitelist): 5 minutes.
//   - Convert: the service's OWN answer, keyed by (from currency, units, nanos, to currency), 60 seconds.
//     The service converts through EUR with floating point and floors units and nanos separately
//     (src/currency.ts), and its API exposes no rate, so a rate derived from one answer could not
//     reproduce the others without drift. Caching the answers returns byte-identical Money.
//
// Concurrent identical calls collapse into one (singleflight). Errors are never cached. The convert
// map is bounded. The clock is injectable for tests.
const (
	supportedCurrenciesTTL = 5 * time.Minute
	conversionTTL          = 60 * time.Second
	conversionMaxEntries   = 4096
)

var currencyCacheTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "frontend_currency_cache_total",
		Help: "Currency lookups by operation (supported, convert) and result (hit, miss, shared = joined an identical call in flight)",
	},
	[]string{"op", "result"},
)

type convKey struct {
	from  string
	units int64
	nanos int32
	to    string
}

type cachedValue struct {
	val   any
	until time.Time
}

type flight struct {
	done chan struct{}
	val  any
	err  error
}

type currencyCache struct {
	mu      sync.Mutex
	now     func() time.Time // nil = time.Now
	entries map[any]cachedValue
	flights map[any]*flight
}

func (c *currencyCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// get returns the cached value for key, or runs fetch once for all concurrent callers and caches a
// successful result for ttl. fetch runs on a context that survives the cancellation of the caller
// that happened to start it (the others still want the answer), bounded by backendTimeout; every
// caller stops waiting when its own context ends.
func (c *currencyCache) get(ctx context.Context, op string, key any, ttl time.Duration, fetch func(context.Context) (any, error)) (any, error) {
	c.mu.Lock()
	now := c.clock()
	if e, ok := c.entries[key]; ok {
		if now.Before(e.until) {
			c.mu.Unlock()
			currencyCacheTotal.WithLabelValues(op, "hit").Inc()
			return e.val, nil
		}
		delete(c.entries, key)
	}
	if f, ok := c.flights[key]; ok {
		c.mu.Unlock()
		currencyCacheTotal.WithLabelValues(op, "shared").Inc()
		return wait(ctx, f)
	}
	f := &flight{done: make(chan struct{})}
	if c.flights == nil {
		c.flights = map[any]*flight{}
	}
	c.flights[key] = f
	c.mu.Unlock()
	currencyCacheTotal.WithLabelValues(op, "miss").Inc()

	go func() {
		fctx, cancel := callCtx(context.WithoutCancel(ctx))
		defer cancel()
		f.val, f.err = fetch(fctx)
		c.mu.Lock()
		delete(c.flights, key)
		if f.err == nil {
			c.store(key, f.val, ttl)
		}
		c.mu.Unlock()
		close(f.done)
	}()
	return wait(ctx, f)
}

func wait(ctx context.Context, f *flight) (any, error) {
	select {
	case <-f.done:
		return f.val, f.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// store must be called with c.mu held.
func (c *currencyCache) store(key, val any, ttl time.Duration) {
	if c.entries == nil {
		c.entries = map[any]cachedValue{}
	}
	now := c.clock()
	if len(c.entries) >= conversionMaxEntries {
		for k, e := range c.entries { // first drop what is stale
			if !now.Before(e.until) {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= conversionMaxEntries {
			clear(c.entries) // still full of live entries: start over rather than track recency
		}
	}
	c.entries[key] = cachedValue{val: val, until: now.Add(ttl)}
}

type supportedKey struct{}

// currencyAPI is the client of currency-service: the test double or the gRPC client.
func (fe *frontendServer) currencyAPI() pb.CurrencyServiceClient {
	if fe.currencyClient != nil {
		return fe.currencyClient
	}
	return pb.NewCurrencyServiceClient(fe.currencySvcConn)
}

func (fe *frontendServer) getCurrencies(ctx context.Context) ([]string, error) {
	v, err := fe.currency.get(ctx, "supported", supportedKey{}, supportedCurrenciesTTL, func(ctx context.Context) (any, error) {
		currs, err := fe.currencyAPI().GetSupportedCurrencies(ctx, &pb.Empty{})
		if err != nil {
			return nil, err
		}
		var out []string
		for _, c := range currs.CurrencyCodes {
			if _, ok := whitelistedCurrencies[c]; ok {
				out = append(out, c)
			}
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	return append([]string(nil), v.([]string)...), nil // a copy: the caller may sort or edit it
}

func (fe *frontendServer) convertCurrency(ctx context.Context, money *pb.Money, currency string) (*pb.Money, error) {
	if avoidNoopCurrencyConversionRPC && money.GetCurrencyCode() == currency {
		return money, nil
	}
	if money == nil {
		// not cacheable (no key): let the service say what it thinks of it
		return fe.fetchConversion(ctx, money, currency)
	}
	key := convKey{from: money.GetCurrencyCode(), units: money.GetUnits(), nanos: money.GetNanos(), to: currency}
	v, err := fe.currency.get(ctx, "convert", key, conversionTTL, func(ctx context.Context) (any, error) {
		return fe.fetchConversion(ctx, money, currency)
	})
	if err != nil {
		return nil, err
	}
	m := v.(*pb.Money)
	return &pb.Money{CurrencyCode: m.GetCurrencyCode(), Units: m.GetUnits(), Nanos: m.GetNanos()}, nil
}

func (fe *frontendServer) fetchConversion(ctx context.Context, money *pb.Money, currency string) (*pb.Money, error) {
	ctx, cancel := callCtx(ctx)
	defer cancel()
	return fe.currencyAPI().Convert(ctx, &pb.CurrencyConversionRequest{From: money, ToCode: currency})
}
