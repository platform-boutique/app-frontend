package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/gorilla/mux"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "frontend/genproto"
	pbi "frontend/genproto/inventory"
	pbm "frontend/genproto/mailbox"
	pbo "frontend/genproto/orders"
	pbu "frontend/genproto/users"
)

const (
	port            = "8080"
	defaultCurrency = "USD"
	cookieMaxAge    = 60 * 60 * 48

	cookiePrefix    = "shop_"
	cookieSessionID = cookiePrefix + "session-id"
	cookieCurrency  = cookiePrefix + "currency"
	cookieAuth      = cookiePrefix + "auth"
)

var (
	whitelistedCurrencies = map[string]bool{
		"USD": true,
		"EUR": true,
		"CAD": true,
		"JPY": true,
		"GBP": true,
		"TRY": true,
	}

	baseUrl = ""
)

type ctxKeySessionID struct{}

type frontendServer struct {
	productCatalogSvcAddr string
	productCatalogSvcConn *grpc.ClientConn

	currencySvcAddr string
	currencySvcConn *grpc.ClientConn

	cartSvcAddr string
	cartSvcConn *grpc.ClientConn

	recommendationSvcAddr string
	recommendationSvcConn *grpc.ClientConn

	checkoutSvcAddr string
	checkoutSvcConn *grpc.ClientConn

	shippingSvcAddr string
	shippingSvcConn *grpc.ClientConn

	adSvcAddr string
	adSvcConn *grpc.ClientConn

	// optional: nil when REVIEW_SERVICE_ADDR is set to an empty string (reviews off)
	reviewSvcAddr string
	reviewSvcConn *grpc.ClientConn
	reviews       pb.ReviewServiceClient

	// optional: nil when ORDER_SERVICE_ADDR is set to an empty string (order history and tracking off)
	orderSvcAddr string
	orderSvcConn *grpc.ClientConn
	orders       pbo.OrderServiceClient

	// optional: nil when USER_SERVICE_ADDR is set to an empty string (accounts off)
	userSvcAddr string
	userSvcConn *grpc.ClientConn
	users       pbu.UserServiceClient

	// optional: nil when INVENTORY_SERVICE_ADDR is set to an empty string (the catalogue's stock is used)
	inventorySvcAddr string
	inventorySvcConn *grpc.ClientConn
	inventory        pbi.InventoryServiceClient

	// optional: nil when EMAIL_SERVICE_ADDR is set to an empty string (the mailbox is off); it also needs the orders
	emailSvcAddr string
	emailSvcConn *grpc.ClientConn
	mailbox      pbm.MailboxServiceClient

	nav navCache
}

func main() {
	ctx := context.Background()

	log := logrus.New()
	log.Level = logrus.DebugLevel
	log.Formatter = &logrus.JSONFormatter{
		FieldMap: logrus.FieldMap{
			logrus.FieldKeyTime:  "timestamp",
			logrus.FieldKeyLevel: "severity",
			logrus.FieldKeyMsg:   "message",
		},
		TimestampFormat: time.RFC3339Nano,
	}
	log.Out = os.Stdout

	// --- Browser RUM (Grafana Faro): validated first so a typo fails the start, not the first page ---
	faroCfg, err := faroSettingsFromEnv(os.Getenv)
	if err != nil {
		log.Fatalf("invalid configuration: %v", err)
	}
	faro = faroCfg
	if faro != nil {
		log.Infof("Faro RUM enabled: app %q version %q env %q, collector %s, session sample rate %v",
			faro.AppName, faro.AppVersion, faro.Env, faro.CollectorURL, faro.SampleRate)
	} else {
		log.Info("FARO_COLLECTOR_URL is empty: browser RUM (Faro) is off")
	}

	// --- Observability ---
	initMetrics()
	startMetricsServer(log)

	// --- Telemetry: traces, metrics and logs pushed over OTLP/gRPC (Alloy → Tempo/Mimir/Loki) ---
	shutdownTelemetry, err := initTelemetry(ctx, "frontend", log)
	if err != nil {
		log.Warnf("telemetry disabled, continuing without it: %v", err)
	} else {
		log.Info("telemetry enabled (OTLP/gRPC)")
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = shutdownTelemetry(shutdownCtx)
		}()
	}

	if os.Getenv("ENABLE_PROFILING") == "1" {
		log.Info("Profiling enabled.")
		go initProfiling(log)
	} else {
		log.Info("Profiling disabled.")
	}

	// --- Service wiring ---
	svc := new(frontendServer)
	baseUrl = os.Getenv("BASE_URL")

	mustMapEnv(&svc.productCatalogSvcAddr, "PRODUCT_CATALOG_SERVICE_ADDR")
	mustMapEnv(&svc.currencySvcAddr, "CURRENCY_SERVICE_ADDR")
	mustMapEnv(&svc.cartSvcAddr, "CART_SERVICE_ADDR")
	mustMapEnv(&svc.recommendationSvcAddr, "RECOMMENDATION_SERVICE_ADDR")
	mustMapEnv(&svc.checkoutSvcAddr, "CHECKOUT_SERVICE_ADDR")
	mustMapEnv(&svc.shippingSvcAddr, "SHIPPING_SERVICE_ADDR")
	mustMapEnv(&svc.adSvcAddr, "AD_SERVICE_ADDR")

	mustConnGRPC(&svc.currencySvcConn, svc.currencySvcAddr)
	mustConnGRPC(&svc.productCatalogSvcConn, svc.productCatalogSvcAddr)
	mustConnGRPC(&svc.cartSvcConn, svc.cartSvcAddr)
	mustConnGRPC(&svc.recommendationSvcConn, svc.recommendationSvcAddr)
	mustConnGRPC(&svc.shippingSvcConn, svc.shippingSvcAddr)
	mustConnGRPC(&svc.checkoutSvcConn, svc.checkoutSvcAddr)
	mustConnGRPC(&svc.adSvcConn, svc.adSvcAddr)

	// review-service is optional: lazy connection, pages render without ratings while it is down
	if svc.reviewSvcAddr = reviewAddrFromEnv(); svc.reviewSvcAddr != "" {
		mustConnGRPC(&svc.reviewSvcConn, svc.reviewSvcAddr)
		svc.reviews = pb.NewReviewServiceClient(svc.reviewSvcConn)
	} else {
		log.Info("REVIEW_SERVICE_ADDR is empty: ratings and reviews are off")
	}

	// order-service is optional too: lazy connection, only /orders and /orders/{id} depend on it
	if svc.orderSvcAddr = ordersAddrFromEnv(); svc.orderSvcAddr != "" {
		mustConnGRPC(&svc.orderSvcConn, svc.orderSvcAddr)
		svc.orders = pbo.NewOrderServiceClient(svc.orderSvcConn)
		ordersEnabled = true
	} else {
		log.Info("ORDER_SERVICE_ADDR is empty: order history and tracking are off")
	}

	// user-service is optional as well: lazy connection, only the account pages and the identity of a request depend on it
	if svc.userSvcAddr = usersAddrFromEnv(); svc.userSvcAddr != "" {
		mustConnGRPC(&svc.userSvcConn, svc.userSvcAddr)
		svc.users = pbu.NewUserServiceClient(svc.userSvcConn)
		accountsEnabled = true
	} else {
		log.Info("USER_SERVICE_ADDR is empty: accounts are off")
	}

	// inventory-service is optional as well: lazy connection, without it (or while it is down) the catalogue's stock is shown
	if svc.inventorySvcAddr = inventoryAddrFromEnv(); svc.inventorySvcAddr != "" {
		mustConnGRPC(&svc.inventorySvcConn, svc.inventorySvcAddr)
		svc.inventory = pbi.NewInventoryServiceClient(svc.inventorySvcConn)
	} else {
		log.Info("INVENTORY_SERVICE_ADDR is empty: live stock is off, the catalogue's stock is used")
	}

	// email-service (the mailbox) is optional as well: lazy connection, only /emails and the links to it depend on it.
	// Which mails are the visitor's is told by order-service, so the mailbox needs the orders too.
	if svc.emailSvcAddr = emailAddrFromEnv(); svc.emailSvcAddr == "" {
		log.Info("EMAIL_SERVICE_ADDR is empty: the mailbox is off")
	} else if svc.orders == nil {
		log.Info("ORDER_SERVICE_ADDR is empty: the mailbox is off (it needs the orders to know whose mail is whose)")
	} else {
		mustConnGRPC(&svc.emailSvcConn, svc.emailSvcAddr)
		svc.mailbox = pbm.NewMailboxServiceClient(svc.emailSvcConn)
		mailboxEnabled = true
	}

	// --- Router ---
	r := mux.NewRouter()
	r.HandleFunc(baseUrl+"/", svc.homeHandler).Methods(http.MethodGet, http.MethodHead)
	r.HandleFunc(baseUrl+"/product/{id}", svc.productHandler).Methods(http.MethodGet, http.MethodHead)
	r.HandleFunc(baseUrl+"/product/{id}/reviews", svc.postReviewHandler).Methods(http.MethodPost)
	r.HandleFunc(baseUrl+"/cart", svc.viewCartHandler).Methods(http.MethodGet, http.MethodHead)
	r.HandleFunc(baseUrl+"/cart", svc.addToCartHandler).Methods(http.MethodPost)
	r.HandleFunc(baseUrl+"/cart/empty", svc.emptyCartHandler).Methods(http.MethodPost)
	r.HandleFunc(baseUrl+"/setCurrency", svc.setCurrencyHandler).Methods(http.MethodPost)
	r.HandleFunc(baseUrl+"/logout", svc.logoutHandler).Methods(http.MethodGet)
	svc.accountRoutes(r)
	r.HandleFunc(baseUrl+"/orders", svc.ordersHistoryHandler).Methods(http.MethodGet, http.MethodHead)
	r.HandleFunc(baseUrl+"/orders/{id}", svc.orderTrackingHandler).Methods(http.MethodGet, http.MethodHead)
	r.HandleFunc(baseUrl+"/emails", svc.emailsHandler).Methods(http.MethodGet, http.MethodHead)
	r.HandleFunc(baseUrl+"/emails/{id}", svc.emailHandler).Methods(http.MethodGet, http.MethodHead)
	r.HandleFunc(baseUrl+"/cart/checkout", svc.placeOrderHandler).Methods(http.MethodPost)
	r.HandleFunc(baseUrl+"/ads/{id}/click", svc.adClickHandler).Methods(http.MethodGet)
	r.PathPrefix(baseUrl + "/static/").Handler(http.StripPrefix(baseUrl+"/static/", http.FileServer(http.Dir("./static/"))))
	r.HandleFunc(baseUrl+"/robots.txt", func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, "User-agent: *\nDisallow: /") })
	r.HandleFunc(baseUrl+"/_healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, "ok") })
	r.HandleFunc(baseUrl+"/product-meta/{ids}", svc.getProductByID).Methods(http.MethodGet)

	// Wrap all routes with Prometheus metrics middleware
	wrapRoutes(r)

	var handler http.Handler = r
	handler = svc.withUser(handler)                    // who is asking: shop_auth cookie -> user-service GetSession
	handler = &logHandler{log: log, next: handler}     // structured logging
	handler = ensureSessionID(handler)                 // session ID cookie
	handler = otelhttp.NewHandler(handler, "frontend") // OTel tracing

	srvPort := port
	if p := os.Getenv("PORT"); p != "" {
		srvPort = p
	}
	addr := os.Getenv("LISTEN_ADDR")

	log.Infof("starting server on %s:%s", addr, srvPort)
	srv := &http.Server{
		Addr:         addr + ":" + srvPort,
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}

func mustMapEnv(target *string, envKey string) {
	v := os.Getenv(envKey)
	if v == "" {
		panic(fmt.Sprintf("environment variable %q not set", envKey))
	}
	*target = v
}

// mustConnGRPC creates a lazy client connection (no dial here); the deadline of each
// call is set where it is made, see rpc.go.
func mustConnGRPC(conn **grpc.ClientConn, addr string) {
	var err error
	*conn, err = grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
	)
	if err != nil {
		panic(errors.Wrapf(err, "grpc: failed to connect %s", addr))
	}
}
