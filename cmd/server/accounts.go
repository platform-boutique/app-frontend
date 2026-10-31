package main

import (
	"context"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gorilla/mux"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "frontend/genproto"
	pbo "frontend/genproto/orders"
	pbu "frontend/genproto/users"
)

// Accounts: register, sign in, sign out, /account, backed by user-service.
//
// Like reviews and orders the service is optional. With USER_SERVICE_ADDR set to an empty
// string the routes answer 404 and the header has no account link; while the service is down
// only the pages that need it answer a friendly 503 page, the rest of the shop keeps working
// as an anonymous visitor.
//
// Identity: a signed-in visitor's cart key is the user id (see cartKey), an anonymous one's is
// the session cookie. The token of the session lives only in the shop_auth cookie (HttpOnly):
// it is never put into a template, a log line or an error text.

const (
	defaultUserAddr = "user-service:8086"

	// GetSession is on the path of every request that carries the cookie: keep it short
	userSessionTimeout = time.Second
	// bcrypt makes register and login slow on purpose
	userAuthTimeout   = 5 * time.Second
	userLogoutTimeout = 2 * time.Second
	// claiming orders and merging the cart after a sign-in, all together
	afterLoginTimeout = 8 * time.Second
	claimTimeout      = 1500 * time.Millisecond

	// the service's limits (users.proto), checked here first so a typo is explained next to its field
	minPasswordBytes = 8
	maxPasswordBytes = 72
	maxEmailChars    = 254
	maxUserName      = 100

	// the service does not tell how long the limit lasts; its window is about a minute
	retryAfterSeconds = 60

	defaultTokenLifetime = 14 * 24 * time.Hour // used only if the service sends no expiry

	msgAccountsUnavailable = "Accounts are not available right now. Please try again in a moment. You can keep shopping as a guest."
	msgWrongCredentials    = "Wrong email or password."
	msgTooManyAttempts     = "Too many attempts. Please wait a minute and try again."
	msgDuplicateEmail      = "An account with this email already exists."
)

// accountsEnabled tells the header whether to show the Sign in / account link (set once in main, like ordersEnabled).
var accountsEnabled bool

// usersAddrFromEnv returns the user-service address: USER_SERVICE_ADDR if set, the default
// otherwise. A variable that is set but empty switches accounts off.
func usersAddrFromEnv() string {
	if v, ok := os.LookupEnv("USER_SERVICE_ADDR"); ok {
		return strings.TrimSpace(v)
	}
	return defaultUserAddr
}

// usersUnavailableError marks "user-service did not give a usable answer"; classifyError
// turns it into the friendly 503 page.
type usersUnavailableError struct{ cause error }

func (e *usersUnavailableError) Error() string {
	return "user service unavailable: " + e.cause.Error()
}
func (e *usersUnavailableError) Unwrap() error { return e.cause }

// ---------------------------------------------------------------------------
// identity of a request
// ---------------------------------------------------------------------------

// accountUser is the signed-in visitor. It never holds the token.
type accountUser struct {
	ID, Email, Name string
}

// identity is what the auth middleware found out about a request.
type identity struct {
	user        *accountUser
	unavailable bool // there was a cookie but user-service could not say whom it belongs to
}

type ctxKeyIdentity struct{}

func identityOf(r *http.Request) identity {
	id, _ := r.Context().Value(ctxKeyIdentity{}).(identity)
	return id
}

// userOf returns the signed-in user, nil for an anonymous visitor.
func userOf(r *http.Request) *accountUser { return identityOf(r).user }

// cartKey is the key of the visitor's cart (and of recommendations): the user id when signed
// in, the session cookie otherwise. PlaceOrderRequest.user_id is this key.
func cartKey(r *http.Request) string {
	if u := userOf(r); u != nil {
		return u.ID
	}
	return sessionID(r)
}

// tokenPattern is the shape of a session token (32 random bytes, base64url without padding is
// 43 characters); anything else cannot be valid and is not even sent to the service.
var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

func authCookie(token string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     cookieAuth,
		Value:    token,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	}
}

func expireAuthCookie(w http.ResponseWriter) {
	c := authCookie("", -1)
	c.Expires = time.Unix(0, 0)
	http.SetCookie(w, c)
}

// authToken returns the token from the cookie, "" when there is none.
func authToken(r *http.Request) string {
	if c, err := r.Cookie(cookieAuth); err == nil {
		return c.Value
	}
	return ""
}

// withUser is the middleware that resolves the shop_auth cookie, once per request, and puts the
// identity into the context. No cookie: anonymous, no call. A cookie the service does not know
// (stale, revoked, logged out elsewhere) or one that cannot be a token: expired and the request
// goes on as anonymous. A service that cannot answer: anonymous for this request, the cookie is
// kept (an outage must not sign everybody out) and the pages that need the identity say so.
// It must run inside logHandler (it needs the request logger).
func (fe *frontendServer) withUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fe.users == nil || isStaticPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		token := authToken(r)
		if token == "" {
			next.ServeHTTP(w, r)
			return
		}
		log, _ := r.Context().Value(ctxKeyLog{}).(logrus.FieldLogger)
		if log == nil {
			log = logrus.New()
		}
		var id identity
		if !tokenPattern.MatchString(token) {
			log.Debug("auth cookie is not a token, expiring it")
			expireAuthCookie(w)
		} else {
			ctx, cancel := context.WithTimeout(r.Context(), userSessionTimeout)
			u, err := fe.users.GetSession(ctx, &pbu.GetSessionRequest{Token: token})
			cancel()
			switch {
			case err == nil && u.GetId() != "":
				id.user = &accountUser{ID: u.GetId(), Email: u.GetEmail(), Name: u.GetName()}
				log = log.WithField("user", u.GetId())
			case err == nil || status.Code(err) == codes.Unauthenticated || status.Code(err) == codes.NotFound:
				log.Debug("auth cookie is stale, expiring it")
				expireAuthCookie(w)
			default:
				log.WithField("error", err).Warn("could not resolve the session, treating the request as anonymous")
				id.unavailable = true
			}
		}
		ctx := context.WithValue(r.Context(), ctxKeyIdentity{}, id)
		ctx = context.WithValue(ctx, ctxKeyLog{}, log)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// isStaticPath: assets and probes never need to know who is asking.
func isStaticPath(p string) bool {
	return strings.HasPrefix(p, baseUrl+"/static/") || p == baseUrl+"/_healthz" || p == baseUrl+"/robots.txt"
}

// identityUnavailable answers 503 for a page that cannot be right without knowing the visitor
// (orders, checkout, account) when a cookie is present but user-service could not resolve it.
func (fe *frontendServer) identityUnavailable(log logrus.FieldLogger, r *http.Request, w http.ResponseWriter) bool {
	if !identityOf(r).unavailable {
		return false
	}
	fe.renderHTTPError(log, r, w, &usersUnavailableError{cause: errors.New("session could not be resolved")}, http.StatusServiceUnavailable)
	return true
}

// ---------------------------------------------------------------------------
// safe redirects
// ---------------------------------------------------------------------------

// safeNext returns next if it is a local path the visitor may be sent to after signing in, ""
// otherwise. Accepted: starts with a single "/", not "//" and not "/\" (browsers read both as a
// host), no backslash and no control character anywhere (browsers drop tabs and newlines inside a
// URL, so "/\t/evil.com" would become "//evil.com"), parses as a URL without scheme and host.
func safeNext(next string) string {
	if next == "" || len(next) > 512 || next[0] != '/' {
		return ""
	}
	if strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") || strings.ContainsRune(next, '\\') {
		return ""
	}
	for _, c := range next {
		if c < 0x20 || c == 0x7f {
			return ""
		}
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil {
		return ""
	}
	return next
}

// ---------------------------------------------------------------------------
// forms
// ---------------------------------------------------------------------------

// authForm is the state of the sign-in and registration forms. The password is never part of it:
// it is not rendered back, whatever happens.
type authForm struct {
	Name, Email string
	Next        string            // safe local path or ""
	Errors      map[string]string // field -> message; "form" for errors not tied to a field
}

func (f authForm) HasErrors() bool { return len(f.Errors) > 0 }

func (f *authForm) fail(field, msg string) {
	if f.Errors == nil {
		f.Errors = map[string]string{}
	}
	if _, ok := f.Errors[field]; !ok {
		f.Errors[field] = msg
	}
}

func (f *authForm) validateEmail() {
	f.Email = strings.TrimSpace(f.Email)
	if f.Email == "" {
		f.fail("email", "Please enter your email address.")
		return
	}
	// a plain address, as the service requires (no "Name <a@b>" form)
	if a, err := mail.ParseAddress(f.Email); err != nil || a.Address != f.Email || len(f.Email) < 3 || utf8.RuneCountInString(f.Email) > maxEmailChars || !strings.Contains(f.Email[strings.LastIndex(f.Email, "@"):], ".") {
		f.fail("email", "Please enter a valid email address, like name@example.com.")
	}
}

func (f *authForm) validateRegister(password string) {
	f.Name = strings.TrimSpace(f.Name)
	switch n := utf8.RuneCountInString(f.Name); {
	case n == 0:
		f.fail("name", "Please enter your name.")
	case n > maxUserName:
		f.fail("name", "Your name can be at most "+strconv.Itoa(maxUserName)+" characters.")
	case strings.ContainsRune(f.Name, 0):
		f.fail("name", "Your name contains characters that are not allowed.")
	}
	f.validateEmail()
	switch {
	case len(password) < minPasswordBytes:
		f.fail("password", "Your password must be at least "+strconv.Itoa(minPasswordBytes)+" characters.")
	case len(password) > maxPasswordBytes:
		f.fail("password", "Your password can be at most "+strconv.Itoa(maxPasswordBytes)+" bytes (about "+strconv.Itoa(maxPasswordBytes)+" characters; accented letters and emoji count for more).")
	}
}

func (f *authForm) validateLogin(password string) {
	f.validateEmail()
	if f.Errors["email"] != "" && strings.TrimSpace(f.Email) != "" {
		// a malformed address cannot belong to an account; same sentence as a wrong password (no hint)
		delete(f.Errors, "email")
		f.fail("form", msgWrongCredentials)
	}
	if password == "" {
		f.fail("password", "Please enter your password.")
	}
}

// ---------------------------------------------------------------------------
// handlers
// ---------------------------------------------------------------------------

// accountRoutes registers the account pages (main.go calls it, so do the tests).
func (fe *frontendServer) accountRoutes(r *mux.Router) {
	r.HandleFunc(baseUrl+"/register", fe.registerFormHandler).Methods(http.MethodGet, http.MethodHead)
	r.HandleFunc(baseUrl+"/register", fe.registerHandler).Methods(http.MethodPost)
	r.HandleFunc(baseUrl+"/login", fe.loginFormHandler).Methods(http.MethodGet, http.MethodHead)
	r.HandleFunc(baseUrl+"/login", fe.loginHandler).Methods(http.MethodPost)
	r.HandleFunc(baseUrl+"/account", fe.accountHandler).Methods(http.MethodGet, http.MethodHead)
	r.HandleFunc(baseUrl+"/logout", fe.logoutPostHandler).Methods(http.MethodPost)
}

// accountsOff answers 404 when accounts are switched off.
func (fe *frontendServer) accountsOff(log logrus.FieldLogger, r *http.Request, w http.ResponseWriter) bool {
	if fe.users != nil {
		return false
	}
	fe.renderHTTPError(log, r, w, errors.New("accounts are not enabled"), http.StatusNotFound)
	return true
}

func (fe *frontendServer) renderAuthPage(w http.ResponseWriter, r *http.Request, name, title string, form authForm, code int) {
	log := r.Context().Value(ctxKeyLog{}).(logrus.FieldLogger)
	if code == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
	}
	// nothing on these pages may be cached: they hold what was typed
	w.Header().Set("Cache-Control", "no-store")
	if code != http.StatusOK {
		w.WriteHeader(code)
	}
	if err := getTemplates().ExecuteTemplate(w, name, injectCommonTemplateData(r, map[string]interface{}{
		"title":      title,
		"categories": fe.navCategories(r.Context()),
		"form":       form,
		"register":   name == "register_page",
	})); err != nil {
		log.Println(err)
	}
}

// alreadySignedIn sends a signed-in visitor on to the account page instead of showing the forms.
func alreadySignedIn(w http.ResponseWriter, r *http.Request) bool {
	if userOf(r) == nil {
		return false
	}
	http.Redirect(w, r, baseUrl+"/account", http.StatusSeeOther)
	return true
}

func (fe *frontendServer) loginFormHandler(w http.ResponseWriter, r *http.Request) {
	log := r.Context().Value(ctxKeyLog{}).(logrus.FieldLogger)
	if fe.accountsOff(log, r, w) || alreadySignedIn(w, r) {
		return
	}
	fe.renderAuthPage(w, r, "login_page", "Sign in", authForm{Next: safeNext(r.URL.Query().Get("next"))}, http.StatusOK)
}

func (fe *frontendServer) registerFormHandler(w http.ResponseWriter, r *http.Request) {
	log := r.Context().Value(ctxKeyLog{}).(logrus.FieldLogger)
	if fe.accountsOff(log, r, w) || alreadySignedIn(w, r) {
		return
	}
	fe.renderAuthPage(w, r, "register_page", "Create an account", authForm{Next: safeNext(r.URL.Query().Get("next"))}, http.StatusOK)
}

// readAuthForm checks the origin and reads the posted form. ok is false when it already answered.
//
// CSRF: like the other forms there is no token. The cookies are SameSite=Strict, and browsers
// that send Sec-Fetch-Site are refused when the post comes from another site (which also stops
// somebody from signing a visitor in to the attacker's account).
func (fe *frontendServer) readAuthForm(log logrus.FieldLogger, w http.ResponseWriter, r *http.Request) (ok bool) {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		fe.renderHTTPError(log, r, w, errors.New("cross-site form post refused"), http.StatusForbidden)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		fe.renderHTTPError(log, r, w, errors.Wrap(err, "could not read the form"), http.StatusBadRequest)
		return false
	}
	return true
}

func (fe *frontendServer) loginHandler(w http.ResponseWriter, r *http.Request) {
	log := r.Context().Value(ctxKeyLog{}).(logrus.FieldLogger)
	if fe.accountsOff(log, r, w) {
		return
	}
	if !fe.readAuthForm(log, w, r) || alreadySignedIn(w, r) {
		return
	}
	password := r.PostFormValue("password")
	form := authForm{Email: r.PostFormValue("email"), Next: safeNext(r.PostFormValue("next"))}
	form.validateLogin(password)
	if form.HasErrors() {
		fe.renderAuthPage(w, r, "login_page", "Sign in", form, http.StatusUnprocessableEntity)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), userAuthTimeout)
	defer cancel()
	resp, err := fe.users.Login(ctx, &pbu.LoginRequest{Email: form.Email, Password: password})
	if err != nil {
		fe.authFailed(w, r, log, "login_page", "Sign in", form, err, "login failed")
		return
	}
	log.WithField("user", resp.GetUser().GetId()).Info("signed in")
	fe.finishSignIn(w, r, log, resp, form.Next)
}

func (fe *frontendServer) registerHandler(w http.ResponseWriter, r *http.Request) {
	log := r.Context().Value(ctxKeyLog{}).(logrus.FieldLogger)
	if fe.accountsOff(log, r, w) {
		return
	}
	if !fe.readAuthForm(log, w, r) || alreadySignedIn(w, r) {
		return
	}
	password := r.PostFormValue("password")
	form := authForm{Name: r.PostFormValue("name"), Email: r.PostFormValue("email"), Next: safeNext(r.PostFormValue("next"))}
	form.validateRegister(password)
	if form.HasErrors() {
		fe.renderAuthPage(w, r, "register_page", "Create an account", form, http.StatusUnprocessableEntity)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), userAuthTimeout)
	defer cancel()
	resp, err := fe.users.Register(ctx, &pbu.RegisterRequest{Email: form.Email, Password: password, Name: form.Name})
	if err != nil {
		fe.authFailed(w, r, log, "register_page", "Create an account", form, err, "registration failed")
		return
	}
	log.WithField("user", resp.GetUser().GetId()).Info("account created")
	fe.finishSignIn(w, r, log, resp, form.Next)
}

// validationMessage is the service's INVALID_ARGUMENT text for the form. It names the field
// ("email: ..."), which is wanted here, so unlike readableMessage it keeps that prefix.
func validationMessage(msg string) string {
	msg = strings.TrimSpace(rpcWrapper.ReplaceAllString(strings.TrimSpace(msg), ""))
	if msg == "" {
		return msgUnprocessable
	}
	r, n := utf8.DecodeRuneInString(msg)
	return strings.ToUpper(string(r)) + msg[n:]
}

// authFailed maps the error of Register / Login to the form (the contract, section 4):
// INVALID_ARGUMENT -> 422 with the service's message, ALREADY_EXISTS -> 422, UNAUTHENTICATED ->
// 422 "wrong email or password" and nothing else, RESOURCE_EXHAUSTED -> 429 + Retry-After, any
// other failure (down, slow) -> the 503 page. The error text itself only goes to the log.
func (fe *frontendServer) authFailed(w http.ResponseWriter, r *http.Request, log logrus.FieldLogger, page, title string, form authForm, err error, what string) {
	log.WithField("code", status.Code(err).String()).Info(what)
	code := http.StatusUnprocessableEntity
	switch status.Code(err) {
	case codes.InvalidArgument:
		form.fail("form", validationMessage(status.Convert(err).Message()))
	case codes.AlreadyExists:
		form.fail("email", msgDuplicateEmail)
	case codes.Unauthenticated:
		form.fail("form", msgWrongCredentials)
	case codes.ResourceExhausted:
		code = http.StatusTooManyRequests
		form.fail("form", msgTooManyAttempts)
	default:
		fe.renderHTTPError(log, r, w, &usersUnavailableError{cause: errors.Wrap(err, what)}, http.StatusServiceUnavailable)
		return
	}
	fe.renderAuthPage(w, r, page, title, form, code)
}

// finishSignIn runs after a successful Register or Login: sets the cookie, hands the orders and
// the cart of the anonymous session to the user, and redirects. Neither handover can fail the
// sign-in: the account exists and the cookie is set, so errors are logged and the visitor goes on.
func (fe *frontendServer) finishSignIn(w http.ResponseWriter, r *http.Request, log logrus.FieldLogger, resp *pbu.AuthResponse, next string) {
	maxAge := int(defaultTokenLifetime / time.Second)
	if exp := resp.GetExpiresAt(); exp != nil {
		maxAge = int(time.Until(exp.AsTime()) / time.Second)
	}
	if maxAge < 1 {
		maxAge = 1
	}
	http.SetCookie(w, authCookie(resp.GetToken(), maxAge))

	userID := resp.GetUser().GetId()
	claimed := 0
	if anon := sessionID(r); anon != "" && anon != userID && !sharedSessionMode() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), afterLoginTimeout)
		defer cancel()
		claimed = fe.claimOrders(ctx, log, anon, userID)
		fe.mergeCart(ctx, log, anon, userID)
	}

	dest := baseUrl + "/account"
	switch {
	case next != "":
		dest = next
	case claimed > 0 && fe.orders != nil:
		dest = baseUrl + "/orders"
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// sharedSessionMode is the demo switch where every visitor has the same session id: the
// "anonymous" orders and cart of that id belong to everybody, so no user may take them over.
func sharedSessionMode() bool { return os.Getenv("ENABLE_SINGLE_SHARED_SESSION") == "true" }

// claimOrders gives the orders of the anonymous session to the user; failures are logged only.
func (fe *frontendServer) claimOrders(ctx context.Context, log logrus.FieldLogger, anon, userID string) int {
	if fe.orders == nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(ctx, claimTimeout)
	defer cancel()
	resp, err := fe.orders.ClaimOrders(ctx, &pbo.ClaimOrdersRequest{SessionId: anon, UserId: userID})
	if err != nil {
		log.WithField("error", err).Warn("could not claim the orders of the session, signing in anyway")
		return 0
	}
	log.WithField("claimed", resp.GetClaimed()).Info("orders claimed")
	return int(resp.GetClaimed())
}

// mergeCart moves the anonymous cart into the user's: every line is added to the user's cart
// capped by the stock (what the user's cart already holds counts), then the anonymous cart is
// emptied. It is not atomic (cart-service has no such call): if it stops half way the anonymous
// cart is left as it is, so nothing is lost, and the lines already added are capped again by the
// stock at the next sign-in. Failures are logged only.
func (fe *frontendServer) mergeCart(ctx context.Context, log logrus.FieldLogger, anon, userID string) {
	anonCart, err := fe.getCart(ctx, anon)
	if err != nil {
		log.WithField("error", err).Warn("could not read the anonymous cart, signing in anyway")
		return
	}
	if len(anonCart) == 0 {
		return
	}
	userCart, err := fe.getCart(ctx, userID)
	if err != nil {
		log.WithField("error", err).Warn("could not read the user's cart, leaving the anonymous cart as it is")
		return
	}
	have := map[string]int64{}
	for _, it := range userCart {
		have[it.GetProductId()] += int64(it.GetQuantity())
	}
	want := map[string]int64{}
	var order []string
	for _, it := range anonCart {
		if _, seen := want[it.GetProductId()]; !seen {
			order = append(order, it.GetProductId())
		}
		want[it.GetProductId()] += int64(it.GetQuantity())
	}
	// the products first (nothing is added to the user's cart before they are all known), then the
	// live stock of all of them in one call; without inventory-service the catalogue's stock caps the lines
	products := make(map[string]*pb.Product, len(order))
	var found []*pb.Product
	for _, id := range order {
		p, err := fe.getProduct(ctx, id)
		if status.Code(err) == codes.NotFound {
			continue // the product left the catalog: nothing to carry over
		}
		if err != nil {
			log.WithField("error", err).Warn("could not check the stock while merging the cart, leaving the anonymous cart as it is")
			return
		}
		products[id] = p
		found = append(found, p)
	}
	stock := fe.overlayStock(ctx, log, found...)
	added := 0
	for _, id := range order {
		p, ok := products[id]
		if !ok {
			continue
		}
		room := int64(stock.of(p)) - have[id]
		qty := min(want[id], room, int64(1<<31-1))
		if qty <= 0 {
			continue
		}
		if err := fe.insertCart(ctx, userID, id, int32(qty)); err != nil { // #nosec G115 -- capped to int32 above
			log.WithField("error", err).Warn("could not add an item to the user's cart, leaving the anonymous cart as it is")
			return
		}
		added++
	}
	if err := fe.emptyCart(ctx, anon); err != nil {
		log.WithField("error", err).Warn("could not empty the anonymous cart after merging it")
		return
	}
	log.WithField("lines", added).Info("anonymous cart merged")
}

func (fe *frontendServer) accountHandler(w http.ResponseWriter, r *http.Request) {
	log := r.Context().Value(ctxKeyLog{}).(logrus.FieldLogger)
	if fe.accountsOff(log, r, w) || fe.identityUnavailable(log, r, w) {
		return
	}
	u := userOf(r)
	if u == nil {
		http.Redirect(w, r, baseUrl+"/login?next="+url.QueryEscape(baseUrl+"/account"), http.StatusSeeOther)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if err := getTemplates().ExecuteTemplate(w, "account_page", injectCommonTemplateData(r, map[string]interface{}{
		"title":      "Your account",
		"categories": fe.navCategories(r.Context()),
	})); err != nil {
		log.Println(err)
	}
}

// revokeToken asks user-service to end the session; best effort, the cookie is expired by the caller anyway.
func (fe *frontendServer) revokeToken(ctx context.Context, log logrus.FieldLogger, token string) {
	if fe.users == nil || token == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), userLogoutTimeout)
	defer cancel()
	if _, err := fe.users.Logout(ctx, &pbu.LogoutRequest{Token: token}); err != nil {
		log.WithField("error", err).Warn("could not revoke the session at user-service (it expires on its own)")
	}
}

// logoutPostHandler signs out: revoke the token, expire the cookie, back to the start page.
func (fe *frontendServer) logoutPostHandler(w http.ResponseWriter, r *http.Request) {
	log := r.Context().Value(ctxKeyLog{}).(logrus.FieldLogger)
	if fe.accountsOff(log, r, w) {
		return
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		fe.renderHTTPError(log, r, w, errors.New("cross-site form post refused"), http.StatusForbidden)
		return
	}
	fe.revokeToken(r.Context(), log, authToken(r))
	expireAuthCookie(w)
	log.Info("signed out")
	http.Redirect(w, r, baseUrl+"/", http.StatusSeeOther)
}
