package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"math/big"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	pb "frontend/genproto"
	"frontend/internal/validator"
	"frontend/money"
)

var (
	frontendMessage = strings.TrimSpace(os.Getenv("FRONTEND_MESSAGE"))
	brand           = brandFromEnv()
	tmpl            *template.Template
	tmplOnce        sync.Once
)

func getTemplates() *template.Template {
	tmplOnce.Do(func() {
		path := os.Getenv("TEMPLATE_PATH")
		if path == "" {
			path = "templates/*.html"
		}
		tmpl = template.Must(template.New("").
			Funcs(template.FuncMap{
				"renderMoney":        renderMoney,
				"renderCurrencyLogo": renderCurrencyLogo,
				"cardData":           cardData,
				"ratingPct":          ratingPct,
				"fmtRating":          fmtRating,
				"add1":               func(i int) int { return i + 1 },
				"ratingChoices":      func() []int { return []int{1, 2, 3, 4, 5} },
				"adHref":             adHref,
			}).ParseGlob(path))
	})
	return tmpl
}

func (fe *frontendServer) homeHandler(w http.ResponseWriter, r *http.Request) {
	log := r.Context().Value(ctxKeyLog{}).(logrus.FieldLogger)
	params := parseBrowseParams(r)
	fragment := isFragmentRequest(r)
	log.WithField("currency", currentCurrency(r)).WithField("category", params.Category).
		WithField("page", params.Page).WithField("fragment", fragment).Info("home")

	// categories, currencies and cart in parallel; the fragment needs neither currencies nor cart
	var (
		currencies []string
		cats       []*pb.Category
		cart       []*pb.CartItem
		currErr    error
		catErr     error
		cartErr    error
	)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); cats, catErr = fe.listCategories(r.Context()) }()
	if !fragment {
		wg.Add(2)
		go func() { defer wg.Done(); currencies, currErr = fe.getCurrencies(r.Context()) }()
		go func() { defer wg.Done(); cart, cartErr = fe.getCart(r.Context(), cartKey(r)) }()
	}
	wg.Wait()

	if catErr != nil {
		log.WithField("error", catErr).Warn("failed to list categories")
	}
	if currErr != nil {
		fe.renderHTTPError(log, r, w, errors.Wrap(currErr, "could not retrieve currencies"), http.StatusInternalServerError)
		return
	}
	if cartErr != nil {
		fe.renderHTTPError(log, r, w, errors.Wrap(cartErr, "could not retrieve cart"), http.StatusInternalServerError)
		return
	}

	products, total, err := fe.browse(r.Context(), &params, cats)
	if err != nil {
		fe.renderHTTPError(log, r, w, errors.Wrap(err, "could not retrieve products"), http.StatusInternalServerError)
		return
	}

	// ratings of the whole page in one call, in parallel with the price conversions
	ids := make([]string, len(products))
	for i, p := range products {
		ids[i] = p.GetId()
	}
	var ratings map[string]ratingView
	var stock liveStock
	wg.Add(2)
	go func() { defer wg.Done(); ratings = fe.getRatingSummaries(r.Context(), log, ids) }()
	go func() { defer wg.Done(); stock = fe.overlayStock(r.Context(), log, products...) }()

	// convert all product prices in parallel
	type priceResult struct {
		price *pb.Money
		err   error
	}
	prices := make([]priceResult, len(products))
	for i, p := range products {
		wg.Add(1)
		go func(i int, p *pb.Product) {
			defer wg.Done()
			price, err := fe.convertCurrency(r.Context(), p.GetPriceUsd(), currentCurrency(r))
			prices[i] = priceResult{price, err}
		}(i, p)
	}
	wg.Wait()

	ps := make([]productCard, len(products))
	for i, p := range products {
		if prices[i].err != nil {
			fe.renderHTTPError(log, r, w, errors.Wrapf(prices[i].err, "failed to do currency conversion for product %s", p.GetId()), http.StatusInternalServerError)
			return
		}
		ps[i] = productCard{Item: p, Price: prices[i].price, Image: cardImage(p), DeptName: deptName(cats, p.GetDepartment()), Stock: stock.info(p)}
		if rv, ok := ratings[p.GetId()]; ok {
			ps[i].Rating = &rv
		}
	}

	data := map[string]interface{}{
		"show_currency": true,
		"currencies":    currencies,
		"products":      ps,
		"total":         total,
		"categories":    cats,
		"browse":        params,
		"category":      findCategory(cats, params.Category),
		"sort_options":  sortOptions,
		"pagination":    buildPagination(params, total),
		"cart_size":     fe.headerCartSize(r.Context(), log, cart),
	}
	data["title"] = params.pageTitle(findCategory(cats, params.Category))
	name := "home"
	if fragment {
		name = "catalog_fragment"
		w.Header().Set("Vary", "HX-Request")
		// the view changed more than the results (hero, heading, breadcrumbs): load the full page
		if cur, ok := currentBrowseParams(r); ok && !params.sameLayout(cur) {
			w.Header().Set("HX-Redirect", params.url(params.Page))
			return
		}
	} else {
		data["ad"] = fe.chooseAd(r.Context(), adContextKeys(params), log)
	}
	if err := getTemplates().ExecuteTemplate(w, name, injectCommonTemplateData(r, data)); err != nil {
		log.Error(err)
	}
}

func (fe *frontendServer) productHandler(w http.ResponseWriter, r *http.Request) {
	log := r.Context().Value(ctxKeyLog{}).(logrus.FieldLogger)
	id := mux.Vars(r)["id"]
	if id == "" {
		fe.renderHTTPError(log, r, w, errors.New("product id not specified"), http.StatusBadRequest)
		return
	}
	// htmx "load more" of the reviews: only the next items, not the whole page
	if isFragmentRequest(r) && r.URL.Query().Has("rpage") {
		fe.reviewsFragment(w, r, id)
		return
	}
	fe.renderProduct(w, r, id, nil, http.StatusOK)
}

// renderProduct renders the product page. form is the review form to show with its
// errors after a rejected post (nil = empty form); code is the HTTP status.
func (fe *frontendServer) renderProduct(w http.ResponseWriter, r *http.Request, id string, form *reviewForm, code int) {
	log := r.Context().Value(ctxKeyLog{}).(logrus.FieldLogger)
	log.WithField("id", id).WithField("currency", currentCurrency(r)).Debug("serving product page")

	// fetch product, currencies, and cart in parallel
	var (
		p          *pb.Product
		currencies []string
		cart       []*pb.CartItem
		reviews    reviewsSection
		prodErr    error
		currErr    error
		cartErr    error
	)
	var wg sync.WaitGroup
	wg.Add(4)
	go func() { defer wg.Done(); reviews = fe.loadReviews(r.Context(), log, id, reviewPageParam(r), true) }()
	go func() { defer wg.Done(); p, prodErr = fe.getProduct(r.Context(), id) }()
	go func() { defer wg.Done(); currencies, currErr = fe.getCurrencies(r.Context()) }()
	go func() { defer wg.Done(); cart, cartErr = fe.getCart(r.Context(), cartKey(r)) }()
	wg.Wait()

	if prodErr != nil {
		fe.renderHTTPError(log, r, w, errors.Wrap(prodErr, "could not retrieve product"), http.StatusInternalServerError)
		return
	}
	if currErr != nil {
		fe.renderHTTPError(log, r, w, errors.Wrap(currErr, "could not retrieve currencies"), http.StatusInternalServerError)
		return
	}
	if cartErr != nil {
		fe.renderHTTPError(log, r, w, errors.Wrap(cartErr, "could not retrieve cart"), http.StatusInternalServerError)
		return
	}

	// price conversion and recommendations both depend on product, run in parallel
	var (
		price           *pb.Money
		recommendations []*pb.Product
		cats            []*pb.Category
		priceErr        error
	)
	var stock liveStock
	wg.Add(4)
	go func() { defer wg.Done(); stock = fe.overlayStock(r.Context(), log, p) }()
	go func() {
		defer wg.Done()
		var err error
		if cats, err = fe.listCategories(r.Context()); err != nil {
			log.WithField("error", err).Warn("failed to list categories")
		}
	}()
	go func() {
		defer wg.Done()
		price, priceErr = fe.convertCurrency(r.Context(), p.GetPriceUsd(), currentCurrency(r))
	}()
	go func() {
		defer wg.Done()
		var err error
		recommendations, err = fe.getRecommendations(r.Context(), cartKey(r), []string{id})
		if err != nil {
			log.WithField("error", err).Warn("failed to get product recommendations")
		}
	}()
	wg.Wait()

	if priceErr != nil {
		fe.renderHTTPError(log, r, w, errors.Wrap(priceErr, "failed to convert currency"), http.StatusInternalServerError)
		return
	}

	product := productPage{Item: p, Price: price, Images: productImages(p), Category: findCategory(cats, p.GetDepartment()), Stock: stock.info(p), Quantities: quantityOptions(stock.of(p))}
	if form != nil {
		reviews.Form = *form
	}
	reviews.Posted = r.URL.Query().Get("reviewed") == "1"

	if code != http.StatusOK {
		w.WriteHeader(code)
	}
	if err := getTemplates().ExecuteTemplate(w, "product", injectCommonTemplateData(r, map[string]interface{}{
		"title":           p.GetName(),
		"ad":              fe.chooseAd(r.Context(), p.Categories, log),
		"show_currency":   true,
		"currencies":      currencies,
		"product":         product,
		"categories":      cats,
		"recommendations": recommendations,
		"reviews":         reviews,
		"cart_size":       fe.headerCartSize(r.Context(), log, cart),
	})); err != nil {
		log.Println(err)
	}
}

func (fe *frontendServer) addToCartHandler(w http.ResponseWriter, r *http.Request) {
	log := r.Context().Value(ctxKeyLog{}).(logrus.FieldLogger)
	quantity, _ := strconv.ParseUint(r.FormValue("quantity"), 10, 32)
	productID := r.FormValue("product_id")
	payload := validator.AddToCartPayload{
		Quantity:  quantity,
		ProductID: productID,
	}
	if err := payload.Validate(); err != nil {
		fe.renderHTTPError(log, r, w, validator.ValidationErrorResponse(err), http.StatusUnprocessableEntity)
		return
	}
	log.WithField("product", payload.ProductID).WithField("quantity", payload.Quantity).Debug("adding to cart")

	p, err := fe.getProduct(r.Context(), payload.ProductID)
	if err != nil {
		fe.renderHTTPError(log, r, w, errors.Wrap(err, "could not retrieve product"), http.StatusInternalServerError)
		return
	}
	if payload.Quantity > math.MaxInt32 {
		fe.renderHTTPError(log, r, w, errors.New("quantity exceeds maximum allowed value"), http.StatusBadRequest)
		return
	}
	// one cart can not hold more than is free right now: the live stock when inventory-service answers, else the catalogue's
	stock := fe.overlayStock(r.Context(), log, p).of(p)
	if stock <= 0 {
		fe.renderHTTPError(log, r, w, errors.Errorf("%s is out of stock.", p.GetName()), http.StatusUnprocessableEntity)
		return
	}
	cart, err := fe.getCart(r.Context(), cartKey(r))
	if err != nil {
		fe.renderHTTPError(log, r, w, errors.Wrap(err, "could not retrieve cart"), http.StatusInternalServerError)
		return
	}
	inCart := int64(0)
	for _, item := range cart {
		if item.GetProductId() == p.GetId() {
			inCart += int64(item.GetQuantity())
		}
	}
	if inCart+int64(payload.Quantity) > int64(stock) {
		msg := errors.Errorf("Only %d of %s in stock.", stock, p.GetName())
		if inCart > 0 {
			msg = errors.Errorf("Only %d of %s in stock, and %d already in your cart.", stock, p.GetName(), inCart)
		}
		fe.renderHTTPError(log, r, w, msg, http.StatusUnprocessableEntity)
		return
	}
	if err := fe.insertCart(r.Context(), cartKey(r), p.GetId(), int32(payload.Quantity)); err != nil { // #nosec G115 -- bounds checked above
		fe.renderHTTPError(log, r, w, errors.Wrap(err, "failed to add to cart"), http.StatusInternalServerError)
		return
	}
	w.Header().Set("location", baseUrl+"/cart")
	w.WriteHeader(http.StatusFound)
}

func (fe *frontendServer) emptyCartHandler(w http.ResponseWriter, r *http.Request) {
	log := r.Context().Value(ctxKeyLog{}).(logrus.FieldLogger)
	log.Debug("emptying cart")
	if err := fe.emptyCart(r.Context(), cartKey(r)); err != nil {
		fe.renderHTTPError(log, r, w, errors.Wrap(err, "failed to empty cart"), http.StatusInternalServerError)
		return
	}
	w.Header().Set("location", baseUrl+"/")
	w.WriteHeader(http.StatusFound)
}

func (fe *frontendServer) viewCartHandler(w http.ResponseWriter, r *http.Request) {
	log := r.Context().Value(ctxKeyLog{}).(logrus.FieldLogger)
	log.Debug("view user cart")
	// the page holds the checkout form and its idempotency key: Back must not restore an old form (and its spent key)
	w.Header().Set("Cache-Control", "no-store")

	// fetch currencies and cart in parallel
	var (
		currencies []string
		cart       []*pb.CartItem
		currErr    error
		cartErr    error
	)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); currencies, currErr = fe.getCurrencies(r.Context()) }()
	go func() { defer wg.Done(); cart, cartErr = fe.getCart(r.Context(), cartKey(r)) }()
	wg.Wait()

	if currErr != nil {
		fe.renderHTTPError(log, r, w, errors.Wrap(currErr, "could not retrieve currencies"), http.StatusInternalServerError)
		return
	}
	if cartErr != nil {
		fe.renderHTTPError(log, r, w, errors.Wrap(cartErr, "could not retrieve cart"), http.StatusInternalServerError)
		return
	}

	// products of the cart lines; lines whose product left the catalog are skipped
	lines, stale, err := fe.liveCartLines(r.Context(), log, cart)
	if err != nil {
		fe.renderHTTPError(log, r, w, err, http.StatusInternalServerError)
		return
	}
	liveCart := linesItems(lines)

	// recommendations, shipping quote and price conversions are independent, run in parallel
	var (
		recommendations []*pb.Product
		shippingCost    *pb.Money
		shippingErr     error
	)
	prices := make([]*pb.Money, len(lines))
	priceErrs := make([]error, len(lines))
	var stock liveStock
	wg.Add(3)
	go func() {
		defer wg.Done()
		products := make([]*pb.Product, len(lines))
		for i, l := range lines {
			products[i] = l.Item
		}
		stock = fe.overlayStock(r.Context(), log, products...)
	}()
	go func() {
		defer wg.Done()
		var err error
		recommendations, err = fe.getRecommendations(r.Context(), cartKey(r), cartIDs(liveCart))
		if err != nil {
			log.WithField("error", err).Warn("failed to get product recommendations")
		}
	}()
	go func() {
		defer wg.Done()
		shippingCost, shippingErr = fe.getShippingQuote(r.Context(), liveCart, currentCurrency(r))
	}()
	for i, l := range lines {
		wg.Add(1)
		go func(i int, p *pb.Product) {
			defer wg.Done()
			prices[i], priceErrs[i] = fe.convertCurrency(r.Context(), p.GetPriceUsd(), currentCurrency(r))
		}(i, l.Item)
	}
	wg.Wait()

	if shippingErr != nil {
		fe.renderHTTPError(log, r, w, errors.Wrap(shippingErr, "failed to get shipping quote"), http.StatusInternalServerError)
		return
	}

	type cartItemView struct {
		Item     *pb.Product
		Quantity int32
		Price    *pb.Money
		Left     int32 // free stock of the product (live when inventory-service answers)
		Short    bool  // the line holds more than is left: checkout would refuse it
		Low      bool  // 1..lowStockLimit left, the line still fits
	}
	items := make([]cartItemView, len(lines))
	totalPrice := &pb.Money{CurrencyCode: currentCurrency(r)}
	for i, l := range lines {
		if priceErrs[i] != nil {
			fe.renderHTTPError(log, r, w, errors.Wrapf(priceErrs[i], "could not convert currency for product #%s", l.Item.GetId()), http.StatusInternalServerError)
			return
		}
		multPrice := money.MultiplySlow(prices[i], uint32(l.Quantity)) // #nosec G115 -- quantity is a non-negative proto int32
		left := stock.of(l.Item)
		items[i] = cartItemView{Item: l.Item, Quantity: l.Quantity, Price: multPrice, Left: left,
			Short: l.Quantity > left, Low: l.Quantity <= left && left <= lowStockLimit}
		totalPrice = money.Must(money.Sum(totalPrice, multPrice))
	}
	totalPrice = money.Must(money.Sum(totalPrice, shippingCost))
	year := time.Now().Year()

	if err := getTemplates().ExecuteTemplate(w, "cart", injectCommonTemplateData(r, map[string]interface{}{
		"currencies":       currencies,
		"recommendations":  recommendations,
		"cart_size":        linesSize(lines),
		"title":            "Cart",
		"stale_items":      stale,
		"categories":       fe.navCategories(r.Context()),
		"shipping_cost":    shippingCost,
		"show_currency":    true,
		"total_cost":       totalPrice,
		"items":            items,
		"expiration_years": []int{year, year + 1, year + 2, year + 3, year + 4},
		"idempotency_key":  newIdempotencyKey(),
		"countries":        checkoutCountries,
		"us_country":       usCountry,
	})); err != nil {
		log.Println(err)
	}
}

func (fe *frontendServer) placeOrderHandler(w http.ResponseWriter, r *http.Request) {
	log := r.Context().Value(ctxKeyLog{}).(logrus.FieldLogger)
	log.Debug("placing order")
	if fe.identityUnavailable(log, r, w) {
		return // a signed-in visitor whose cart we cannot reach must not end up ordering as a guest
	}

	var (
		email         = r.FormValue("email")
		streetAddress = r.FormValue("street_address")
		zipCode, _    = strconv.ParseInt(r.FormValue("zip_code"), 10, 32)
		city          = r.FormValue("city")
		state         = r.FormValue("state")
		country       = r.FormValue("country")
		ccNumber      = r.FormValue("credit_card_number")
		ccMonth, _    = strconv.ParseInt(r.FormValue("credit_card_expiration_month"), 10, 32)
		ccYear, _     = strconv.ParseInt(r.FormValue("credit_card_expiration_year"), 10, 32)
		ccCVV, _      = strconv.ParseInt(r.FormValue("credit_card_cvv"), 10, 32)
	)

	// the form only asks for a state in the United States: normalise the spelling, and drop a state posted for any other country
	if isUS(country) {
		country = usCountry
	} else {
		state = ""
	}

	idemKey := strings.TrimSpace(r.FormValue("idempotency_key"))
	if !validIdempotencyKey(idemKey) {
		fe.renderCheckoutError(log, r, w, errors.New("The checkout form is out of date. Please open your cart again and place the order from there."), http.StatusUnprocessableEntity)
		return
	}

	payload := validator.PlaceOrderPayload{
		Email:         email,
		StreetAddress: streetAddress,
		ZipCode:       zipCode,
		City:          city,
		State:         state,
		Country:       country,
		CcNumber:      ccNumber,
		CcMonth:       ccMonth,
		CcYear:        ccYear,
		CcCVV:         ccCVV,
	}
	if err := payload.Validate(); err != nil {
		fe.renderCheckoutError(log, r, w, validator.ValidationErrorResponse(err), http.StatusUnprocessableEntity)
		return
	}

	orderCtx, cancelOrder := context.WithTimeout(r.Context(), checkoutTimeout)
	defer cancelOrder()
	order, err := pb.NewCheckoutServiceClient(fe.checkoutSvcConn).
		PlaceOrder(orderCtx, &pb.PlaceOrderRequest{
			Email: payload.Email,
			CreditCard: &pb.CreditCardInfo{
				CreditCardNumber:          payload.CcNumber,
				CreditCardExpirationMonth: int32(payload.CcMonth), // #nosec G115 -- parsed with ParseInt bitSize 32, fits in int32
				CreditCardExpirationYear:  int32(payload.CcYear),  // #nosec G115 -- parsed with ParseInt bitSize 32, fits in int32
				CreditCardCvv:             int32(payload.CcCVV),   // #nosec G115 -- parsed with ParseInt bitSize 32, fits in int32
			},
			UserId:         cartKey(r),
			AccountId:      accountID(r),
			IdempotencyKey: idemKey,
			UserCurrency:   currentCurrency(r),
			Address: &pb.Address{
				StreetAddress: payload.StreetAddress,
				City:          payload.City,
				State:         payload.State,
				ZipCode:       int32(payload.ZipCode), // #nosec G115 -- parsed with ParseInt bitSize 32, fits in int32
				Country:       payload.Country,
			},
		})
	if err != nil {
		fe.renderCheckoutError(log, r, w, errors.Wrap(err, "failed to complete the order"), http.StatusInternalServerError)
		return
	}
	log.WithField("order", order.GetOrder().GetOrderId()).Info("order placed")

	// fetch recommendations and currencies in parallel after order completes
	var (
		recommendations []*pb.Product
		currencies      []string
		currErr         error
	)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); recommendations, _ = fe.getRecommendations(r.Context(), cartKey(r), nil) }()
	go func() { defer wg.Done(); currencies, currErr = fe.getCurrencies(r.Context()) }()
	wg.Wait()

	if currErr != nil {
		fe.renderHTTPError(log, r, w, errors.Wrap(currErr, "could not retrieve currencies"), http.StatusInternalServerError)
		return
	}

	totalPaid := order.GetOrder().GetShippingCost()
	for _, v := range order.GetOrder().GetItems() {
		multPrice := money.MultiplySlow(v.GetCost(), uint32(v.GetItem().GetQuantity())) // #nosec G115 -- quantity is a non-negative proto int32
		totalPaid = money.Must(money.Sum(totalPaid, multPrice))
	}

	if err := getTemplates().ExecuteTemplate(w, "order", injectCommonTemplateData(r, map[string]interface{}{
		"title":           "Order complete",
		"show_currency":   false,
		"currencies":      currencies,
		"categories":      fe.navCategories(r.Context()),
		"order":           order.GetOrder(),
		"total_paid":      &totalPaid,
		"recommendations": recommendations,
	})); err != nil {
		log.Println(err)
	}
}

func (fe *frontendServer) logoutHandler(w http.ResponseWriter, r *http.Request) {
	log := r.Context().Value(ctxKeyLog{}).(logrus.FieldLogger)
	log.Debug("logging out")
	if token := authToken(r); token != "" {
		fe.revokeToken(r.Context(), log, token)
		expireAuthCookie(w)
	}
	for _, c := range r.Cookies() {
		if c.Name == cookieAuth {
			continue // expired above, with its own flags
		}
		c.Expires = time.Now().Add(-time.Hour * 24 * 365)
		c.MaxAge = -1
		http.SetCookie(w, c)
	}
	w.Header().Set("Location", baseUrl+"/")
	w.WriteHeader(http.StatusFound)
}

func (fe *frontendServer) getProductByID(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["ids"]
	if id == "" {
		return
	}
	p, err := fe.getProduct(r.Context(), id)
	if err != nil {
		return
	}
	jsonData, err := json.Marshal(p)
	if err != nil {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(jsonData)
}

func (fe *frontendServer) setCurrencyHandler(w http.ResponseWriter, r *http.Request) {
	log := r.Context().Value(ctxKeyLog{}).(logrus.FieldLogger)
	cur := r.FormValue("currency_code")
	payload := validator.SetCurrencyPayload{Currency: cur}
	if err := payload.Validate(); err != nil {
		fe.renderHTTPError(log, r, w, validator.ValidationErrorResponse(err), http.StatusUnprocessableEntity)
		return
	}
	log.WithField("curr.new", payload.Currency).WithField("curr.old", currentCurrency(r)).Debug("setting currency")
	if payload.Currency != "" {
		http.SetCookie(w, &http.Cookie{
			Name:     cookieCurrency,
			Value:    payload.Currency,
			MaxAge:   cookieMaxAge,
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteStrictMode,
		})
	}
	referer := r.Header.Get("referer")
	if referer == "" {
		referer = baseUrl + "/"
	}
	w.Header().Set("Location", referer)
	w.WriteHeader(http.StatusFound)
}

func (fe *frontendServer) chooseAd(ctx context.Context, ctxKeys []string, log logrus.FieldLogger) *pb.Ad {
	ads, err := fe.getAd(ctx, ctxKeys)
	if err != nil {
		log.WithField("error", err).Warn("failed to retrieve ads")
		return nil
	}
	if len(ads) == 0 {
		return nil
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(ads))))
	if err != nil {
		log.WithField("error", err).Warn("failed to generate random ad index")
		return ads[0]
	}
	return ads[n.Int64()]
}

func injectCommonTemplateData(r *http.Request, payload map[string]interface{}) map[string]interface{} {
	data := map[string]interface{}{
		"session_id":       sessionID(r),
		"request_id":       r.Context().Value(ctxKeyRequestID{}),
		"user_currency":    currentCurrency(r),
		"brand_name":       brand.Name,
		"brand_tagline":    brand.Tagline,
		"frontendMessage":  frontendMessage,
		"currentYear":      time.Now().Year(),
		"baseUrl":          baseUrl,
		"orders_enabled":   ordersEnabled,
		"mailbox_enabled":  mailboxEnabled,
		"accounts_enabled": accountsEnabled,
	}
	if u := userOf(r); u != nil {
		data["user"] = u
	}
	faroTemplateData(r, data)
	for k, v := range payload {
		data[k] = v
	}
	return data
}

func currentCurrency(r *http.Request) string {
	c, _ := r.Cookie(cookieCurrency)
	if c != nil {
		return c.Value
	}
	return defaultCurrency
}

func sessionID(r *http.Request) string {
	v := r.Context().Value(ctxKeySessionID{})
	if v != nil {
		return v.(string)
	}
	return ""
}

// accountID is the signed-in user's id for PlaceOrderRequest.account_id, "" for an anonymous visitor.
func accountID(r *http.Request) string {
	if u := userOf(r); u != nil {
		return u.ID
	}
	return ""
}

func cartIDs(c []*pb.CartItem) []string {
	out := make([]string, len(c))
	for i, v := range c {
		out[i] = v.GetProductId()
	}
	return out
}

func cartSize(c []*pb.CartItem) int {
	cartSize := 0
	for _, item := range c {
		cartSize += int(item.GetQuantity())
	}
	return cartSize
}

func renderMoney(money *pb.Money) string {
	currencyLogo := renderCurrencyLogo(money.GetCurrencyCode())
	return fmt.Sprintf("%s%d.%02d", currencyLogo, money.GetUnits(), money.GetNanos()/10000000)
}

func renderCurrencyLogo(currencyCode string) string {
	logos := map[string]string{
		"USD": "$",
		"CAD": "$",
		"JPY": "¥",
		"EUR": "€",
		"TRY": "₺",
		"GBP": "£",
	}
	logo := "$"
	if val, ok := logos[currencyCode]; ok {
		logo = val
	}
	return logo
}
