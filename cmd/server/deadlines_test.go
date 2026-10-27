package main

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	pb "frontend/genproto"
)

// connTo serves register on its own bufconn and returns a client connection to it.
func connTo(t *testing.T, register func(*grpc.Server)) *grpc.ClientConn {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	register(srv)
	go func() { _ = srv.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(); srv.Stop() })
	return conn
}

type emptyAds struct {
	pb.UnimplementedAdServiceServer
}

func (emptyAds) GetAds(context.Context, *pb.AdRequest) (*pb.AdResponse, error) {
	return &pb.AdResponse{}, nil
}

func TestChooseAd_EmptyListIsNoAd(t *testing.T) {
	fe := &frontendServer{adSvcConn: connTo(t, func(s *grpc.Server) { pb.RegisterAdServiceServer(s, emptyAds{}) })}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("chooseAd panicked on an empty ad list: %v", r)
		}
	}()
	if ad := fe.chooseAd(context.Background(), nil, logrus.New()); ad != nil {
		t.Errorf("got ad %v, want none", ad)
	}
}

type hangingCurrency struct {
	pb.UnimplementedCurrencyServiceServer
}

func (hangingCurrency) GetSupportedCurrencies(ctx context.Context, _ *pb.Empty) (*pb.GetSupportedCurrenciesResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// A hung backend ends in a 503 page after the call deadline, not in a connection the
// server drops after its 30 s WriteTimeout.
func TestHungBackendEndsIn503(t *testing.T) {
	old := backendTimeout
	backendTimeout = 150 * time.Millisecond
	defer func() { backendTimeout = old }()

	fe := newFakeFrontend(t, sampleCatalog())
	fe.currencySvcConn = connTo(t, func(s *grpc.Server) { pb.RegisterCurrencyServiceServer(s, hangingCurrency{}) })

	done := make(chan int, 1)
	go func() { done <- do(fe, http.MethodGet, "/cart", "", nil).Code }()
	select {
	case code := <-done:
		if code != http.StatusServiceUnavailable {
			t.Errorf("status %d, want 503", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request did not end: no deadline on the currency call")
	}
}

func TestHungCatalogEndsIn503OnHomeAndProduct(t *testing.T) {
	old := backendTimeout
	backendTimeout = 150 * time.Millisecond
	defer func() { backendTimeout = old }()

	fe := newFakeFrontend(t, sampleCatalog())
	fe.productCatalogSvcConn = connTo(t, func(s *grpc.Server) { pb.RegisterProductCatalogServiceServer(s, hangingCatalog{}) })
	for _, target := range []string{"/", "/product/pan-05"} {
		codes := make(chan int, 1)
		go func() { rr := do(fe, http.MethodGet, target, "", nil); codes <- rr.Code }()
		select {
		case code := <-codes:
			if code != http.StatusServiceUnavailable {
				t.Errorf("%s: status %d, want 503", target, code)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: did not end", target)
		}
	}
}

type hangingCatalog struct {
	pb.UnimplementedProductCatalogServiceServer
}

func (hangingCatalog) ListCategories(ctx context.Context, _ *pb.Empty) (*pb.ListCategoriesResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (hangingCatalog) ListProductsFiltered(ctx context.Context, _ *pb.ListProductsRequest) (*pb.ListProductsResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (hangingCatalog) GetProduct(ctx context.Context, _ *pb.GetProductRequest) (*pb.Product, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
