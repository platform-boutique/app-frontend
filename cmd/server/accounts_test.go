package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "frontend/genproto"
	pbo "frontend/genproto/orders"
	pbu "frontend/genproto/users"
)

// ---------------------------------------------------------------------------
// fake user client (implements pbu.UserServiceClient, in memory, no network)
// ---------------------------------------------------------------------------

type fakeUser struct{ id, email, name, password string }

type fakeUsers struct {
	mu       sync.Mutex
	users    map[string]*fakeUser // by e-mail
	sessions map[string]*fakeUser // by token
	next     int

	registerCalls []*pbu.RegisterRequest
	loginCalls    []*pbu.LoginRequest
	sessionCalls  []string // tokens asked for
	logouts       []string // tokens revoked

	err         error // returned by GetSession when set (the service is down)
	registerErr error // returned by Register when set
	loginErr    error // returned by Login when set
	logoutErr   error
}

func newFakeUsers() *fakeUsers {
	return &fakeUsers{users: map[string]*fakeUser{}, sessions: map[string]*fakeUser{}}
}

// signIn creates the user (if new) and a session for it, like a login that happened earlier.
func (f *fakeUsers) signIn(email, name, password string) (u *fakeUser, token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.signInLocked(email, name, password)
}

func (f *fakeUsers) signInLocked(email, name, password string) (*fakeUser, string) {
	u := f.users[email]
	if u == nil {
		f.next++
		u = &fakeUser{id: fmt.Sprintf("00000000-0000-4000-8000-%012d", f.next), email: email, name: name, password: password}
		f.users[email] = u
	}
	f.next++
	token := fmt.Sprintf("tok_%038d", f.next) // 42 characters of [A-Za-z0-9_]
	f.sessions[token] = u
	return u, token
}

func (u *fakeUser) pb() *pbu.User {
	return &pbu.User{Id: u.id, Email: u.email, Name: u.name, CreatedAt: timestamppb.Now()}
}

func (f *fakeUsers) Register(_ context.Context, in *pbu.RegisterRequest, _ ...grpc.CallOption) (*pbu.AuthResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registerCalls = append(f.registerCalls, in)
	if f.registerErr != nil {
		return nil, f.registerErr
	}
	if _, taken := f.users[in.Email]; taken {
		return nil, status.Error(codes.AlreadyExists, "email taken")
	}
	u, token := f.signInLocked(in.Email, in.Name, in.Password)
	return &pbu.AuthResponse{User: u.pb(), Token: token, ExpiresAt: timestamppb.New(time.Now().Add(14 * 24 * time.Hour))}, nil
}

func (f *fakeUsers) Login(_ context.Context, in *pbu.LoginRequest, _ ...grpc.CallOption) (*pbu.AuthResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loginCalls = append(f.loginCalls, in)
	if f.loginErr != nil {
		return nil, f.loginErr
	}
	u := f.users[in.Email]
	if u == nil || u.password != in.Password { // one message for both, like the service
		return nil, status.Error(codes.Unauthenticated, "wrong email or password")
	}
	_, token := f.signInLocked(in.Email, u.name, u.password)
	return &pbu.AuthResponse{User: u.pb(), Token: token, ExpiresAt: timestamppb.New(time.Now().Add(14 * 24 * time.Hour))}, nil
}

func (f *fakeUsers) GetSession(_ context.Context, in *pbu.GetSessionRequest, _ ...grpc.CallOption) (*pbu.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessionCalls = append(f.sessionCalls, in.Token)
	if f.err != nil {
		return nil, f.err
	}
	u := f.sessions[in.Token]
	if u == nil {
		return nil, status.Error(codes.Unauthenticated, "unknown session")
	}
	return u.pb(), nil
}

func (f *fakeUsers) Logout(_ context.Context, in *pbu.LogoutRequest, _ ...grpc.CallOption) (*pbu.LogoutResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logouts = append(f.logouts, in.Token)
	if f.logoutErr != nil {
		return nil, f.logoutErr
	}
	delete(f.sessions, in.Token)
	return &pbu.LogoutResponse{}, nil
}

// ---------------------------------------------------------------------------
// harness: the real middleware chain of main.go over fake backends
// ---------------------------------------------------------------------------

type acctEnv struct {
	fe    *frontendServer
	users *fakeUsers
	ord   *fakeOrders
	cart  *fakeCart
	co    *fakeCheckout
	logs  *bytes.Buffer // everything the server logged
	h     http.Handler
}

func setAccountsEnabled(t *testing.T, v bool) {
	t.Helper()
	old := accountsEnabled
	accountsEnabled = v
	t.Cleanup(func() { accountsEnabled = old })
}

// newAcct builds a frontend with accounts, orders, per-user carts and the sample catalog.
func newAcct(t *testing.T) *acctEnv {
	t.Helper()
	setAccountsEnabled(t, true)
	setOrdersEnabled(t, true)
	e := &acctEnv{users: newFakeUsers(), ord: &fakeOrders{}, cart: &fakeCart{carts: map[string][]*pb.CartItem{}}, co: &fakeCheckout{}, logs: &bytes.Buffer{}}
	e.fe = newFakeFrontendWith(t, sampleCatalog(), e.cart, e.co)
	e.fe.users, e.fe.orders = e.users, e.ord

	r := mux.NewRouter()
	r.HandleFunc("/", e.fe.homeHandler).Methods(http.MethodGet)
	r.HandleFunc("/cart", e.fe.viewCartHandler).Methods(http.MethodGet)
	r.HandleFunc("/cart", e.fe.addToCartHandler).Methods(http.MethodPost)
	r.HandleFunc("/cart/checkout", e.fe.placeOrderHandler).Methods(http.MethodPost)
	r.HandleFunc("/logout", e.fe.logoutHandler).Methods(http.MethodGet)
	r.HandleFunc("/orders", e.fe.ordersHistoryHandler).Methods(http.MethodGet, http.MethodHead)
	r.HandleFunc("/orders/{id}", e.fe.orderTrackingHandler).Methods(http.MethodGet, http.MethodHead)
	e.fe.accountRoutes(r)

	log := logrus.New()
	log.SetOutput(e.logs)
	log.SetLevel(logrus.DebugLevel)
	e.h = ensureSessionID(&logHandler{log: log, next: e.fe.withUser(r)})
	return e
}

func (e *acctEnv) serve(method, target, body string, cookies []*http.Cookie, header map[string]string) *httptest.ResponseRecorder {
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, target, nil)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	return rr
}

func anon(sid string) []*http.Cookie { return []*http.Cookie{{Name: cookieSessionID, Value: sid}} }

func asUser(sid, token string) []*http.Cookie {
	return []*http.Cookie{{Name: cookieSessionID, Value: sid}, {Name: cookieAuth, Value: token}}
}

// cookieNamed finds the (last) Set-Cookie of that name.
func cookieNamed(rr *httptest.ResponseRecorder, name string) *http.Cookie {
	var found *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == name {
			found = c
		}
	}
	return found
}

func mustRedirect(t *testing.T, rr *httptest.ResponseRecorder, want string) {
	t.Helper()
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != want {
		t.Fatalf("want 303 to %q, got %d to %q; body: %.200s", want, rr.Code, rr.Header().Get("Location"), rr.Body.String())
	}
}

func regForm(extra ...string) string {
	return form(append([]string{"name", "Ada Lovelace", "email", "ada@example.com", "password", "correct horse battery"}, extra...)...)
}

func loginForm(email, pw string, extra ...string) string {
	return form(append([]string{"email", email, "password", pw}, extra...)...)
}

func assertAuthCookieFlags(t *testing.T, c *http.Cookie, token string) {
	t.Helper()
	if c == nil {
		t.Fatal("no shop_auth cookie set")
	}
	if c.Value != token {
		t.Errorf("shop_auth holds %q, want the token", c.Value)
	}
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode {
		t.Errorf("shop_auth flags: HttpOnly=%v Secure=%v SameSite=%v, want HttpOnly, Secure, Strict", c.HttpOnly, c.Secure, c.SameSite)
	}
	if want := 14 * 24 * 3600; c.MaxAge < want-120 || c.MaxAge > want {
		t.Errorf("shop_auth Max-Age %d, want about %d (the token lifetime)", c.MaxAge, want)
	}
}

func isExpired(c *http.Cookie) bool { return c != nil && c.MaxAge < 0 && c.Value == "" }

// ---------------------------------------------------------------------------
// register and login
// ---------------------------------------------------------------------------

func TestRegister_HappyPath_CookieClaimAndRedirect(t *testing.T) {
	e := newAcct(t)
	e.ord.orders = []*pbo.Order{sampleOrder("o-anon", "anon-1", stPlaced, time.Hour)}

	rr := e.serve(http.MethodPost, "/register", regForm(), anon("anon-1"), nil)
	mustRedirect(t, rr, "/orders") // an order was claimed

	if len(e.users.registerCalls) != 1 || e.users.registerCalls[0].Email != "ada@example.com" || e.users.registerCalls[0].Name != "Ada Lovelace" {
		t.Fatalf("register calls: %+v", e.users.registerCalls)
	}
	u := e.users.users["ada@example.com"]
	var token string
	for tok, su := range e.users.sessions {
		if su == u {
			token = tok
		}
	}
	assertAuthCookieFlags(t, cookieNamed(rr, cookieAuth), token)

	// the claim carries the anonymous session id and the user id
	if len(e.ord.claims) != 1 || e.ord.claims[0].SessionId != "anon-1" || e.ord.claims[0].UserId != u.id {
		t.Fatalf("claims: %+v, want (anon-1, %s)", e.ord.claims, u.id)
	}
	if e.ord.orders[0].UserId != u.id {
		t.Error("the order was not given to the user")
	}
}

func TestLogin_HappyPath_NothingToClaimGoesToAccount(t *testing.T) {
	e := newAcct(t)
	e.users.signIn("ada@example.com", "Ada", "correct horse battery")
	e.users.sessions = map[string]*fakeUser{} // the earlier session is gone, only the account stays

	rr := e.serve(http.MethodPost, "/login", loginForm("ada@example.com", "correct horse battery"), anon("anon-1"), nil)
	mustRedirect(t, rr, "/account")
	c := cookieNamed(rr, cookieAuth)
	if c == nil {
		t.Fatal("no cookie")
	}
	assertAuthCookieFlags(t, c, c.Value)
	if _, ok := e.users.sessions[c.Value]; !ok {
		t.Error("the cookie does not hold a token of the service")
	}
	if len(e.ord.claims) != 1 {
		t.Errorf("claim is attempted on every sign-in (idempotent), got %d calls", len(e.ord.claims))
	}
}

func TestLogin_NextIsFollowedOnlyWhenLocal(t *testing.T) {
	cases := []struct{ next, want string }{
		{"/cart", "/cart"},
		{"/orders?page=2", "/orders?page=2"},
		{"//evil.com", "/account"},
		{"//evil.com/orders", "/account"},
		{`/\evil.com`, "/account"},
		{`/\/evil.com`, "/account"},
		{"https://evil.com", "/account"},
		{"https://evil.com/cart", "/account"},
		{"javascript:alert(1)", "/account"},
		{"orders", "/account"},
		{"/\t/evil.com", "/account"},
		{"/\n/evil.com", "/account"},
		{"/ok\r\nSet-Cookie: x=y", "/account"},
		{"", "/account"},
	}
	for _, c := range cases {
		e := newAcct(t)
		e.users.signIn("ada@example.com", "Ada", "correct horse battery")
		rr := e.serve(http.MethodPost, "/login", loginForm("ada@example.com", "correct horse battery", "next", c.next), anon("anon-1"), nil)
		if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != c.want {
			t.Errorf("next=%q: %d to %q, want %q", c.next, rr.Code, rr.Header().Get("Location"), c.want)
		}
	}
}

func TestRegister_NextIsFollowedOnlyWhenLocal(t *testing.T) {
	for next, want := range map[string]string{"/cart": "/cart", "//evil.com": "/account", `/\evil.com`: "/account", "https://evil.com": "/account"} {
		e := newAcct(t)
		rr := e.serve(http.MethodPost, "/register", regForm("next", next), anon("anon-1"), nil)
		if rr.Header().Get("Location") != want {
			t.Errorf("next=%q: Location %q, want %q", next, rr.Header().Get("Location"), want)
		}
	}
}

func TestSafeNext(t *testing.T) {
	good := []string{"/", "/cart", "/orders/abc?x=1#y", "/product/pan-01", "/a//b"}
	bad := []string{"", "//evil.com", `/\evil.com`, "https://evil.com", "http://evil.com/", "evil.com", "/\\", "/%5Cevil.com/../..\\x", "javascript:alert(1)", "/\x00", "/\t/evil.com", "/ \\evil.com", "/" + strings.Repeat("a", 600), " /cart", "\\\\evil.com"}
	for _, s := range good {
		if safeNext(s) != s {
			t.Errorf("safeNext(%q) refused", s)
		}
	}
	for _, s := range bad {
		if safeNext(s) != "" {
			t.Errorf("safeNext(%q) = %q, want it refused", s, safeNext(s))
		}
	}
}

func TestLoginPage_DoesNotEchoUnsafeNext(t *testing.T) {
	e := newAcct(t)
	for _, n := range []string{"//evil.com", `/\evil.com`, "https://evil.com"} {
		html := e.serve(http.MethodGet, "/login?next="+url.QueryEscape(n), "", anon("anon-1"), nil).Body.String()
		if strings.Contains(html, "evil.com") {
			t.Errorf("next=%q reached the page", n)
		}
	}
	html := e.serve(http.MethodGet, "/login?next=%2Fcart", "", anon("anon-1"), nil).Body.String()
	assertContains(t, html, `<input type="hidden" name="next" value="/cart">`, `href="/register?next=%2Fcart"`)
}

func TestLogin_WrongPasswordAndUnknownEmailAreIndistinguishable(t *testing.T) {
	e := newAcct(t)
	e.users.signIn("ada@example.com", "Ada", "correct horse battery")
	wrong := e.serve(http.MethodPost, "/login", loginForm("ada@example.com", "not the password"), anon("anon-1"), nil)
	unknown := e.serve(http.MethodPost, "/login", loginForm("nobody@example.com", "not the password"), anon("anon-1"), nil)
	for name, rr := range map[string]*httptest.ResponseRecorder{"wrong password": wrong, "unknown email": unknown} {
		if rr.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status %d, want 422", name, rr.Code)
		}
		assertContains(t, rr.Body.String(), msgWrongCredentials, `role="alert"`)
		if cookieNamed(rr, cookieAuth) != nil {
			t.Errorf("%s: a cookie was set", name)
		}
		if strings.Contains(rr.Body.String(), "not the password") {
			t.Errorf("%s: the password was echoed", name)
		}
	}
	// apart from the e-mail typed back into the field, the two pages are the same
	norm := func(rr *httptest.ResponseRecorder, email string) string {
		return strings.ReplaceAll(rr.Body.String(), email, "EMAIL")
	}
	strip := func(s string) string { // the request id and session id differ per request
		var out []string
		for _, l := range strings.Split(s, "\n") {
			if !strings.Contains(l, "request-id") {
				out = append(out, l)
			}
		}
		return strings.Join(out, "\n")
	}
	if strip(norm(wrong, "ada@example.com")) != strip(norm(unknown, "nobody@example.com")) {
		t.Error("the two failures render differently (user enumeration)")
	}
}

func TestLogin_MalformedEmailGivesTheSameGenericMessage(t *testing.T) {
	e := newAcct(t)
	rr := e.serve(http.MethodPost, "/login", loginForm("not-an-email", "whatever1"), anon("anon-1"), nil)
	if rr.Code != http.StatusUnprocessableEntity || !strings.Contains(rr.Body.String(), msgWrongCredentials) || len(e.users.loginCalls) != 0 {
		t.Errorf("status %d, login calls %d", rr.Code, len(e.users.loginCalls))
	}
}

func TestLogin_EmptyFieldsAreExplainedInline(t *testing.T) {
	e := newAcct(t)
	rr := e.serve(http.MethodPost, "/login", loginForm("", ""), anon("anon-1"), nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d", rr.Code)
	}
	assertContains(t, rr.Body.String(), "Please enter your email address.", "Please enter your password.",
		`aria-invalid="true"`, `aria-describedby="auth-email-error"`, `id="auth-email-error"`, `for="auth-email"`, `for="auth-password"`)
	if len(e.users.loginCalls) != 0 {
		t.Error("the service was called with an empty form")
	}
}

func TestRegister_DuplicateEmailIs422(t *testing.T) {
	e := newAcct(t)
	e.users.signIn("ada@example.com", "Ada", "x")
	rr := e.serve(http.MethodPost, "/register", regForm(), anon("anon-1"), nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d", rr.Code)
	}
	html := rr.Body.String()
	assertContains(t, html, "An account with this email already exists.", `id="auth-email-error"`, `value="ada@example.com"`, `value="Ada Lovelace"`)
	if strings.Contains(html, "correct horse battery") {
		t.Error("the password was echoed")
	}
	if cookieNamed(rr, cookieAuth) != nil {
		t.Error("a cookie was set")
	}
}

func TestRegister_ValidationIsInlineAndNeverCallsTheService(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"name", form("name", "  ", "email", "a@example.com", "password", "longenough1"), []string{"Please enter your name.", `id="auth-name-error"`}},
		{"email", form("name", "A", "email", "Ada <a@example.com>", "password", "longenough1"), []string{"Please enter a valid email address", `id="auth-email-error"`}},
		{"short password", form("name", "A", "email", "a@example.com", "password", "short"), []string{"at least 8 characters", `id="auth-password-error"`}},
		{"long password", form("name", "A", "email", "a@example.com", "password", strings.Repeat("x", 73)), []string{"at most 72 bytes"}},
		{"long name", form("name", strings.Repeat("n", 101), "email", "a@example.com", "password", "longenough1"), []string{"at most 100 characters"}},
	}
	for _, c := range cases {
		e := newAcct(t)
		rr := e.serve(http.MethodPost, "/register", c.body, anon("anon-1"), nil)
		if rr.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status %d", c.name, rr.Code)
		}
		assertContains(t, rr.Body.String(), append(c.want, `aria-invalid="true"`)...)
		if len(e.users.registerCalls) != 0 {
			t.Errorf("%s: the service was called", c.name)
		}
		if strings.Contains(rr.Body.String(), "longenough1") || strings.Contains(rr.Body.String(), strings.Repeat("x", 73)) {
			t.Errorf("%s: the password was echoed", c.name)
		}
	}
}

func TestRegister_ServiceValidationMessageIsShownInTheForm(t *testing.T) {
	e := newAcct(t)
	e.users.registerErr = status.Error(codes.InvalidArgument, "email: must be a plain address")
	rr := e.serve(http.MethodPost, "/register", regForm(), anon("anon-1"), nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d", rr.Code)
	}
	assertContains(t, rr.Body.String(), "Email: must be a plain address", `value="ada@example.com"`)
}

func TestAuth_RateLimitIs429WithRetryAfter(t *testing.T) {
	for _, target := range []string{"/login", "/register"} {
		e := newAcct(t)
		e.users.loginErr = status.Error(codes.ResourceExhausted, "too many attempts")
		e.users.registerErr = status.Error(codes.ResourceExhausted, "too many attempts")
		body := loginForm("ada@example.com", "pw-pw-pw-pw")
		if target == "/register" {
			body = regForm()
		}
		rr := e.serve(http.MethodPost, target, body, anon("anon-1"), nil)
		if rr.Code != http.StatusTooManyRequests {
			t.Fatalf("%s: status %d, want 429", target, rr.Code)
		}
		if ra := rr.Header().Get("Retry-After"); ra != "60" {
			t.Errorf("%s: Retry-After %q", target, ra)
		}
		assertContains(t, rr.Body.String(), msgTooManyAttempts)
		if cookieNamed(rr, cookieAuth) != nil {
			t.Errorf("%s: a cookie was set", target)
		}
	}
}

func TestAuth_CrossSitePostIsRefused(t *testing.T) {
	e := newAcct(t)
	e.users.signIn("ada@example.com", "Ada", "correct horse battery")
	for target, body := range map[string]string{
		"/login": loginForm("ada@example.com", "correct horse battery"), "/register": regForm(), "/logout": "",
	} {
		rr := e.serve(http.MethodPost, target, body, anon("anon-1"), map[string]string{"Sec-Fetch-Site": "cross-site"})
		if rr.Code != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", target, rr.Code)
		}
	}
	if len(e.users.loginCalls)+len(e.users.registerCalls)+len(e.users.logouts) != 0 {
		t.Error("the service was called for a cross-site post")
	}
	// same-origin and same-site (the browser's own form) are fine
	for _, site := range []string{"same-origin", "none", ""} {
		e2 := newAcct(t)
		e2.users.signIn("ada@example.com", "Ada", "correct horse battery")
		rr := e2.serve(http.MethodPost, "/login", loginForm("ada@example.com", "correct horse battery"), anon("anon-1"), map[string]string{"Sec-Fetch-Site": site})
		if rr.Code != http.StatusSeeOther {
			t.Errorf("Sec-Fetch-Site %q: status %d", site, rr.Code)
		}
	}
}

func TestAuthPages_ArePrivateAndAccessible(t *testing.T) {
	e := newAcct(t)
	for _, target := range []string{"/login", "/register"} {
		rr := e.serve(http.MethodGet, target, "", anon("anon-1"), nil)
		if rr.Code != http.StatusOK || rr.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: status %d, Cache-Control %q", target, rr.Code, rr.Header().Get("Cache-Control"))
		}
		html := rr.Body.String()
		assertContains(t, html, `<main id="main"`, `<h1 id="auth-title">`, `for="auth-email"`, `for="auth-password"`, `type="password"`, `autocomplete=`)
	}
	reg := e.serve(http.MethodGet, "/register", "", anon("anon-1"), nil).Body.String()
	assertContains(t, reg, `for="auth-name"`, `autocomplete="new-password"`, `id="auth-password-hint"`, `autocomplete="name"`)
	if !strings.Contains(e.serve(http.MethodGet, "/login", "", anon("anon-1"), nil).Body.String(), `autocomplete="current-password"`) {
		t.Error("the login password field lacks autocomplete=current-password")
	}
}

// ---------------------------------------------------------------------------
// the cookie, the middleware
// ---------------------------------------------------------------------------

func TestMiddleware_GetSessionOncePerRequestAndNeverWithoutCookie(t *testing.T) {
	e := newAcct(t)
	_, token := e.users.signIn("ada@example.com", "Ada", "x")

	if rr := e.serve(http.MethodGet, "/", "", anon("anon-1"), nil); rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	if len(e.users.sessionCalls) != 0 {
		t.Errorf("GetSession called %d times without a cookie", len(e.users.sessionCalls))
	}
	// the home page has many sub-calls; the identity is still asked once
	rr := e.serve(http.MethodGet, "/", "", asUser("anon-1", token), nil)
	if rr.Code != http.StatusOK || len(e.users.sessionCalls) != 1 || e.users.sessionCalls[0] != token {
		t.Errorf("status %d, GetSession calls %v", rr.Code, e.users.sessionCalls)
	}
	assertContains(t, rr.Body.String(), `class="icon-btn account-link"`, `aria-label="Your account, Ada"`, `href="/account"`)
	if cookieNamed(rr, cookieAuth) != nil {
		t.Error("a valid cookie must be left alone")
	}
}

func TestMiddleware_InvalidCookieIsExpiredAndTheRequestGoesOnAnonymous(t *testing.T) {
	for name, bad := range map[string]string{
		"garbage":       "not a token!!",
		"too short":     "abc",
		"unknown token": "tok_" + strings.Repeat("9", 38), // well formed, the service does not know it
		"too long":      strings.Repeat("a", 300),
	} {
		e := newAcct(t)
		rr := e.serve(http.MethodGet, "/", "", asUser("anon-1", bad), nil)
		if rr.Code != http.StatusOK {
			t.Errorf("%s: status %d", name, rr.Code)
		}
		if c := cookieNamed(rr, cookieAuth); !isExpired(c) {
			t.Errorf("%s: cookie not expired: %+v", name, c)
		} else if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode {
			t.Errorf("%s: the expiring cookie lacks the flags", name)
		}
		html := rr.Body.String()
		assertContains(t, html, `href="/login"`, "Sign in")
		if strings.Contains(html, `href="/account"`) {
			t.Errorf("%s: shown as signed in", name)
		}
		if strings.Contains(html, bad) && len(bad) > 3 {
			t.Errorf("%s: the cookie value reached the page", name)
		}
		// malformed values are not even sent to the service
		if name != "unknown token" && len(e.users.sessionCalls) != 0 {
			t.Errorf("%s: GetSession called for something that cannot be a token", name)
		}
		// a cart and an order page work as an anonymous visitor
		_ = e.serve(http.MethodGet, "/orders", "", asUser("anon-1", bad), nil)
		if n := len(e.ord.listCalls); n != 1 || e.ord.listCalls[0].SessionId != "anon-1" || e.ord.listCalls[0].UserId != "" {
			t.Errorf("%s: orders were listed as %+v, want the session", name, e.ord.listCalls)
		}
	}
}

func TestMiddleware_StaticAssetsDoNotAskTheService(t *testing.T) {
	e := newAcct(t)
	_, token := e.users.signIn("ada@example.com", "Ada", "x")
	r := mux.NewRouter()
	r.PathPrefix("/static/").HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("css")) })
	h := e.fe.withUser(r)
	req := httptest.NewRequest(http.MethodGet, "/static/styles/styles.css", nil)
	req.AddCookie(&http.Cookie{Name: cookieAuth, Value: token})
	h.ServeHTTP(httptest.NewRecorder(), req)
	if len(e.users.sessionCalls) != 0 {
		t.Error("GetSession called for a static asset")
	}
}

func TestLogout_PostRevokesAndExpiresTheCookie(t *testing.T) {
	e := newAcct(t)
	_, token := e.users.signIn("ada@example.com", "Ada", "x")
	rr := e.serve(http.MethodPost, "/logout", "", asUser("anon-1", token), nil)
	mustRedirect(t, rr, "/")
	if c := cookieNamed(rr, cookieAuth); !isExpired(c) {
		t.Errorf("cookie not expired: %+v", c)
	}
	if len(e.users.logouts) != 1 || e.users.logouts[0] != token {
		t.Errorf("Logout calls: %v", e.users.logouts)
	}
	// the replayed cookie is now anonymous
	rr = e.serve(http.MethodGet, "/account", "", asUser("anon-1", token), nil)
	mustRedirect(t, rr, "/login?next=%2Faccount")
	if !isExpired(cookieNamed(rr, cookieAuth)) {
		t.Error("the replayed cookie was not expired")
	}
}

func TestLogout_StillExpiresTheCookieWhenTheServiceFails(t *testing.T) {
	e := newAcct(t)
	_, token := e.users.signIn("ada@example.com", "Ada", "x")
	e.users.logoutErr = status.Error(codes.Unavailable, "down")
	rr := e.serve(http.MethodPost, "/logout", "", asUser("anon-1", token), nil)
	mustRedirect(t, rr, "/")
	if !isExpired(cookieNamed(rr, cookieAuth)) {
		t.Error("cookie not expired")
	}
}

func TestLogout_GetWipesAllCookiesAndCallsLogout(t *testing.T) {
	e := newAcct(t)
	_, token := e.users.signIn("ada@example.com", "Ada", "x")
	cookies := append(asUser("anon-1", token), &http.Cookie{Name: cookieCurrency, Value: "EUR"})
	rr := e.serve(http.MethodGet, "/logout", "", cookies, nil)
	if rr.Code != http.StatusFound || rr.Header().Get("Location") != "/" {
		t.Fatalf("status %d to %q", rr.Code, rr.Header().Get("Location"))
	}
	for _, name := range []string{cookieAuth, cookieSessionID, cookieCurrency} {
		if c := cookieNamed(rr, name); c == nil || c.MaxAge >= 0 {
			t.Errorf("cookie %s not expired: %+v", name, c)
		}
	}
	if len(e.users.logouts) != 1 || e.users.logouts[0] != token {
		t.Errorf("Logout calls: %v", e.users.logouts)
	}
	if c := cookieNamed(rr, cookieAuth); c == nil || !c.HttpOnly || !c.Secure {
		t.Errorf("the expiring auth cookie lacks flags: %+v", c)
	}
}

// ---------------------------------------------------------------------------
// the account page and the header
// ---------------------------------------------------------------------------

func TestAccountPage_ShowsDetailsOrdersLinkAndSignOut(t *testing.T) {
	e := newAcct(t)
	_, token := e.users.signIn("ada@example.com", "Ada <b>L</b>", "x")
	rr := e.serve(http.MethodGet, "/account", "", asUser("anon-1", token), nil)
	if rr.Code != http.StatusOK || rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status %d", rr.Code)
	}
	html := rr.Body.String()
	assertContains(t, html, "ada@example.com", "Ada &lt;b&gt;L&lt;/b&gt;", `href="/orders"`, `action="/logout"`, `method="POST"`, "Sign out", "<title>Your account | ")
	if strings.Contains(html, "<b>L</b>") {
		t.Error("the name was not escaped")
	}
	if strings.Contains(html, token) {
		t.Error("the token is in the page")
	}
}

func TestAccountPage_AnonymousIsSentToLogin(t *testing.T) {
	e := newAcct(t)
	mustRedirect(t, e.serve(http.MethodGet, "/account", "", anon("anon-1"), nil), "/login?next=%2Faccount")
}

func TestLoginAndRegisterPages_SignedInVisitorGoesToAccount(t *testing.T) {
	e := newAcct(t)
	_, token := e.users.signIn("ada@example.com", "Ada", "x")
	for _, target := range []string{"/login", "/register"} {
		mustRedirect(t, e.serve(http.MethodGet, target, "", asUser("anon-1", token), nil), "/account")
	}
}

func TestHeader_SignInLinkForAnonymousNameForUser(t *testing.T) {
	e := newAcct(t)
	_, token := e.users.signIn("ada@example.com", "Ada", "x")
	html := e.serve(http.MethodGet, "/", "", anon("anon-1"), nil).Body.String()
	assertContains(t, html, `href="/login"`, `class="icon-btn account-link is-signin"`, "Sign in", "/static/styles/accounts.css")
	html = e.serve(http.MethodGet, "/", "", asUser("anon-1", token), nil).Body.String()
	assertContains(t, html, `href="/account"`, `class="account-link-label" aria-hidden="true">Ada<`)
	if strings.Contains(html, `href="/login"`) {
		t.Error("the Sign in link is shown to a signed-in visitor")
	}
}

// ---------------------------------------------------------------------------
// claim and cart merge
// ---------------------------------------------------------------------------

func TestCartMerge_AnonymousLinesGoToTheUserCappedByStock(t *testing.T) {
	e := newAcct(t)
	u, _ := e.users.signIn("ada@example.com", "Ada", "pw-pw-pw-pw")
	e.users.sessions = map[string]*fakeUser{}
	e.cart.carts["anon-1"] = []*pb.CartItem{
		{ProductId: "pan-05", Quantity: 2},      // stock 20, user has none
		{ProductId: "pan-03", Quantity: 5},      // stock 20, user already holds 18: room for 2
		{ProductId: "pan-02", Quantity: 10},     // stock 3
		{ProductId: "pan-01", Quantity: 1},      // out of stock
		{ProductId: "OLJCESPC7Z", Quantity: 1},  // no longer in the catalog
		{ProductId: "pan-05", Quantity: 1},      // the same product twice: summed
		{ProductId: "cream-1", Quantity: 4},     // stock 4, exactly
		{ProductId: "pan-04", Quantity: 100000}, // far beyond the stock
	}
	e.cart.carts[u.id] = []*pb.CartItem{{ProductId: "pan-03", Quantity: 18}}

	rr := e.serve(http.MethodPost, "/login", loginForm("ada@example.com", "pw-pw-pw-pw"), anon("anon-1"), nil)
	mustRedirect(t, rr, "/account")

	got := map[string]int32{}
	for _, it := range e.cart.carts[u.id] {
		got[it.ProductId] = it.Quantity
	}
	want := map[string]int32{"pan-05": 3, "pan-03": 20, "pan-02": 3, "cream-1": 4, "pan-04": 20}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("user's cart %v, want %v", got, want)
	}
	if _, left := e.cart.carts["anon-1"]; left {
		t.Error("the anonymous cart was not emptied")
	}
	if len(e.cart.emptyKeys) != 1 || e.cart.emptyKeys[0] != "anon-1" {
		t.Errorf("EmptyCart keys: %v", e.cart.emptyKeys)
	}
	for _, key := range e.cart.getKeys {
		if key != "anon-1" && key != u.id {
			t.Errorf("a cart of %q was read", key)
		}
	}
}

func TestCartMerge_EmptyAnonymousCartTouchesNothing(t *testing.T) {
	e := newAcct(t)
	e.users.signIn("ada@example.com", "Ada", "pw-pw-pw-pw")
	e.users.sessions = map[string]*fakeUser{}
	mustRedirect(t, e.serve(http.MethodPost, "/login", loginForm("ada@example.com", "pw-pw-pw-pw"), anon("anon-1"), nil), "/account")
	if len(e.cart.added) != 0 || len(e.cart.emptyKeys) != 0 {
		t.Errorf("added %d, emptied %v", len(e.cart.added), e.cart.emptyKeys)
	}
}

func TestSignIn_ClaimFailureDoesNotFailTheLogin(t *testing.T) {
	e := newAcct(t)
	e.users.signIn("ada@example.com", "Ada", "pw-pw-pw-pw")
	e.users.sessions = map[string]*fakeUser{}
	e.ord.claimErr = status.Error(codes.Unavailable, "order-service is down")
	e.cart.carts["anon-1"] = []*pb.CartItem{{ProductId: "pan-05", Quantity: 1}}

	rr := e.serve(http.MethodPost, "/login", loginForm("ada@example.com", "pw-pw-pw-pw"), anon("anon-1"), nil)
	mustRedirect(t, rr, "/account")
	if cookieNamed(rr, cookieAuth) == nil {
		t.Error("no cookie")
	}
	// the cart is still merged
	if _, left := e.cart.carts["anon-1"]; left {
		t.Error("the cart merge was skipped because the claim failed")
	}
}

func TestSignIn_MergeFailureDoesNotFailTheLoginAndKeepsTheAnonymousCart(t *testing.T) {
	e := newAcct(t)
	e.users.signIn("ada@example.com", "Ada", "pw-pw-pw-pw")
	e.users.sessions = map[string]*fakeUser{}
	e.cart.carts["anon-1"] = []*pb.CartItem{{ProductId: "pan-05", Quantity: 1}}
	e.cart.addErr = status.Error(codes.Unavailable, "cart-service is down")

	rr := e.serve(http.MethodPost, "/login", loginForm("ada@example.com", "pw-pw-pw-pw"), anon("anon-1"), nil)
	mustRedirect(t, rr, "/account")
	if cookieNamed(rr, cookieAuth) == nil {
		t.Error("no cookie")
	}
	if len(e.cart.carts["anon-1"]) != 1 || len(e.cart.emptyKeys) != 0 {
		t.Error("the anonymous cart must stay when the merge stopped half way")
	}
}

func TestSignIn_SharedSessionModeNeverClaimsOrMerges(t *testing.T) {
	t.Setenv("ENABLE_SINGLE_SHARED_SESSION", "true")
	e := newAcct(t)
	e.users.signIn("ada@example.com", "Ada", "pw-pw-pw-pw")
	e.users.sessions = map[string]*fakeUser{}
	e.ord.orders = []*pbo.Order{sampleOrder("o1", "shared", stPlaced, time.Hour)}
	e.cart.carts["shared"] = []*pb.CartItem{{ProductId: "pan-05", Quantity: 1}}
	mustRedirect(t, e.serve(http.MethodPost, "/login", loginForm("ada@example.com", "pw-pw-pw-pw"), anon("shared"), nil), "/account")
	if len(e.ord.claims) != 0 || len(e.cart.added) != 0 || e.ord.orders[0].UserId != "" {
		t.Error("everybody's orders and cart were handed to one user")
	}
}

// ---------------------------------------------------------------------------
// identity in the rest of the shop
// ---------------------------------------------------------------------------

func TestCheckout_CarriesAccountIDOnlyWhenSignedIn(t *testing.T) {
	e := newAcct(t)
	u, token := e.users.signIn("ada@example.com", "Ada", "x")

	rr := e.serve(http.MethodPost, "/cart/checkout", goodOrder, asUser("anon-1", token), nil)
	if rr.Code != http.StatusOK || e.co.last == nil {
		t.Fatalf("status %d: %.200s", rr.Code, rr.Body.String())
	}
	if e.co.last.AccountId != u.id || e.co.last.UserId != u.id {
		t.Errorf("signed in: account_id %q user_id %q, want both %q", e.co.last.AccountId, e.co.last.UserId, u.id)
	}

	rr = e.serve(http.MethodPost, "/cart/checkout", goodOrder, anon("anon-1"), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	if e.co.last.AccountId != "" || e.co.last.UserId != "anon-1" {
		t.Errorf("anonymous: account_id %q user_id %q, want empty and anon-1", e.co.last.AccountId, e.co.last.UserId)
	}
}

func TestCart_KeyIsTheUserWhenSignedIn(t *testing.T) {
	e := newAcct(t)
	u, token := e.users.signIn("ada@example.com", "Ada", "x")
	e.cart.carts[u.id] = []*pb.CartItem{{ProductId: "pan-05", Quantity: 2}}
	e.cart.carts["anon-1"] = []*pb.CartItem{{ProductId: "pan-06", Quantity: 1}}

	html := e.serve(http.MethodGet, "/cart", "", asUser("anon-1", token), nil).Body.String()
	assertContains(t, html, "Pan 05")
	if strings.Contains(html, "Pan 06") {
		t.Error("the anonymous cart is shown to the signed-in user")
	}
	html = e.serve(http.MethodGet, "/cart", "", anon("anon-1"), nil).Body.String()
	assertContains(t, html, "Pan 06")

	// adding to the cart goes to the user's cart
	rr := e.serve(http.MethodPost, "/cart", form("product_id", "pan-07", "quantity", "1"), asUser("anon-1", token), nil)
	if rr.Code != http.StatusFound {
		t.Fatalf("status %d", rr.Code)
	}
	found := false
	for _, it := range e.cart.carts[u.id] {
		found = found || it.ProductId == "pan-07"
	}
	if !found {
		t.Error("the item did not land in the user's cart")
	}
}

func TestOrders_ByUserWhenSignedInBySessionOtherwise(t *testing.T) {
	e := newAcct(t)
	u, token := e.users.signIn("ada@example.com", "Ada", "x")
	mine := sampleOrder("11111111-aaaa-4aaa-8aaa-aaaaaaaaaaaa", u.id, stPlaced, time.Hour)
	mine.UserId = u.id
	anonOrder := sampleOrder("22222222-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "anon-1", stPlaced, time.Hour)
	e.ord.orders = []*pbo.Order{mine, anonOrder}

	rr := e.serve(http.MethodGet, "/orders", "", asUser("anon-1", token), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	last := e.ord.listCalls[len(e.ord.listCalls)-1]
	if last.UserId != u.id || last.SessionId != "" {
		t.Errorf("signed in: ListOrders %+v, want user_id only", last)
	}
	assertContains(t, rr.Body.String(), "11111111")
	if strings.Contains(rr.Body.String(), "22222222") {
		t.Error("an anonymous session's order is listed for the user")
	}

	rr = e.serve(http.MethodGet, "/orders", "", anon("anon-1"), nil)
	last = e.ord.listCalls[len(e.ord.listCalls)-1]
	if last.SessionId != "anon-1" || last.UserId != "" {
		t.Errorf("anonymous: ListOrders %+v, want session_id only", last)
	}
	assertContains(t, rr.Body.String(), "22222222")
}

func TestOrderTracking_GuardIsTheUserWhenSignedIn(t *testing.T) {
	e := newAcct(t)
	u, token := e.users.signIn("ada@example.com", "Ada", "x")
	_, otherToken := e.users.signIn("bob@example.com", "Bob", "x")
	mine := sampleOrder("mine-1", u.id, stPlaced, time.Hour)
	mine.UserId = u.id
	e.ord.orders = []*pbo.Order{mine}

	rr := e.serve(http.MethodGet, "/orders/mine-1", "", asUser("anon-1", token), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	g := e.ord.getCalls[len(e.ord.getCalls)-1]
	if g.UserId != u.id || g.SessionId != "" {
		t.Errorf("GetOrder %+v, want user_id guard only", g)
	}
	// somebody else, signed in on the same browser: the same 404 as for an unknown id
	other := e.serve(http.MethodGet, "/orders/mine-1", "", asUser("anon-1", otherToken), nil)
	unknown := e.serve(http.MethodGet, "/orders/nope-1", "", asUser("anon-1", otherToken), nil)
	if other.Code != http.StatusNotFound || unknown.Code != http.StatusNotFound {
		t.Errorf("foreign order: %d, unknown: %d, want 404 for both", other.Code, unknown.Code)
	}
	// anonymous with the user's id as session id is not the user
	if rr := e.serve(http.MethodGet, "/orders/mine-1", "", anon("anon-1"), nil); rr.Code != http.StatusNotFound {
		t.Errorf("anonymous: status %d, want 404", rr.Code)
	}
	if g := e.ord.getCalls[len(e.ord.getCalls)-1]; g.SessionId != "anon-1" || g.UserId != "" {
		t.Errorf("anonymous GetOrder %+v, want the session guard", g)
	}
	// defence in depth: a service that ignores the guard must not leak the order
	e.fe.orders = &leakyOrders{inner: e.ord}
	if rr := e.serve(http.MethodGet, "/orders/mine-1", "", asUser("anon-1", otherToken), nil); rr.Code != http.StatusNotFound {
		t.Errorf("guard ignored by the service: status %d, want 404", rr.Code)
	}
}

func TestClaimThenOrdersEndToEnd(t *testing.T) {
	e := newAcct(t)
	e.ord.orders = []*pbo.Order{sampleOrder("22222222-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "anon-1", stDelivered, time.Hour)}
	rr := e.serve(http.MethodPost, "/register", regForm(), anon("anon-1"), nil)
	mustRedirect(t, rr, "/orders")
	token := cookieNamed(rr, cookieAuth).Value
	// a fresh browser: another session cookie, the same account
	page := e.serve(http.MethodGet, "/orders", "", asUser("anon-2", token), nil)
	assertContains(t, page.Body.String(), "22222222")
	// a second user on the first browser cannot take the order a second time
	e.serve(http.MethodPost, "/logout", "", asUser("anon-1", token), nil)
	rr = e.serve(http.MethodPost, "/register", form("name", "Bob", "email", "bob@example.com", "password", "another password"), anon("anon-1"), nil)
	mustRedirect(t, rr, "/account")
	bob := e.users.users["bob@example.com"]
	if e.ord.orders[0].UserId == bob.id {
		t.Error("the order moved to a second user")
	}
}

// ---------------------------------------------------------------------------
// accounts off, service down
// ---------------------------------------------------------------------------

func TestAccountsOff_RoutesAre404AndNoLinks(t *testing.T) {
	e := newAcct(t)
	e.fe.users = nil
	setAccountsEnabled(t, false)
	for _, c := range []struct{ method, target, body string }{
		{http.MethodGet, "/login", ""}, {http.MethodGet, "/register", ""}, {http.MethodGet, "/account", ""},
		{http.MethodPost, "/login", loginForm("a@example.com", "pw-pw-pw-pw")}, {http.MethodPost, "/register", regForm()}, {http.MethodPost, "/logout", ""},
	} {
		rr := e.serve(c.method, c.target, c.body, asUser("anon-1", "tok_"+strings.Repeat("1", 38)), nil)
		if rr.Code != http.StatusNotFound {
			t.Errorf("%s %s: status %d, want 404", c.method, c.target, rr.Code)
		}
		if cookieNamed(rr, cookieAuth) != nil {
			t.Errorf("%s %s: touched the auth cookie", c.method, c.target)
		}
	}
	// the shop works, shows no account links, and a stray cookie is ignored
	rr := e.serve(http.MethodGet, "/", "", asUser("anon-1", "tok_"+strings.Repeat("1", 38)), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("home: status %d", rr.Code)
	}
	html := rr.Body.String()
	for _, gone := range []string{`href="/login"`, `href="/account"`, "Sign in", `account-link`} {
		if strings.Contains(html, gone) {
			t.Errorf("home still has %q with accounts off", gone)
		}
	}
	for _, page := range []string{"/cart", "/orders"} {
		html := e.serve(http.MethodGet, page, "", anon("anon-1"), nil).Body.String()
		if strings.Contains(html, `href="/login"`) || strings.Contains(html, "Sign in") {
			t.Errorf("%s has a Sign in link with accounts off", page)
		}
	}
	// GET /logout still works
	if rr := e.serve(http.MethodGet, "/logout", "", anon("anon-1"), nil); rr.Code != http.StatusFound {
		t.Errorf("GET /logout: %d", rr.Code)
	}
}

func TestServiceDown_ShopKeepsWorkingAndAccountPagesSay503(t *testing.T) {
	e := newAcct(t)
	_, token := e.users.signIn("ada@example.com", "Ada", "x")
	e.users.err = status.Error(codes.Unavailable, "connection refused to 10.1.2.3:8086")
	e.users.loginErr = status.Error(codes.Unavailable, "connection refused to 10.1.2.3:8086")
	e.users.registerErr = status.Error(codes.DeadlineExceeded, "slow")

	// the rest of the shop works, as an anonymous visitor, and the cookie is kept
	for _, page := range []string{"/", "/cart"} {
		rr := e.serve(http.MethodGet, page, "", asUser("anon-1", token), nil)
		if rr.Code != http.StatusOK {
			t.Errorf("%s: status %d, want 200", page, rr.Code)
		}
		if cookieNamed(rr, cookieAuth) != nil {
			t.Errorf("%s: the cookie was touched during an outage (it would sign everybody out)", page)
		}
	}
	// the forms open
	if rr := e.serve(http.MethodGet, "/login", "", anon("anon-1"), nil); rr.Code != http.StatusOK {
		t.Errorf("GET /login: status %d", rr.Code)
	}
	// pages that cannot be right without the identity say so
	for name, rr := range map[string]*httptest.ResponseRecorder{
		"login":    e.serve(http.MethodPost, "/login", loginForm("ada@example.com", "x"), anon("anon-1"), nil),
		"register": e.serve(http.MethodPost, "/register", regForm(), anon("anon-1"), nil),
		"account":  e.serve(http.MethodGet, "/account", "", asUser("anon-1", token), nil),
		"orders":   e.serve(http.MethodGet, "/orders", "", asUser("anon-1", token), nil),
		"order":    e.serve(http.MethodGet, "/orders/o-1", "", asUser("anon-1", token), nil),
		"checkout": e.serve(http.MethodPost, "/cart/checkout", goodOrder, asUser("anon-1", token), nil),
	} {
		if rr.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: status %d, want 503", name, rr.Code)
			continue
		}
		html := rr.Body.String()
		assertContains(t, html, "Accounts are not available")
		if strings.Contains(html, "10.1.2.3") || strings.Contains(html, "connection refused") {
			t.Errorf("%s: backend text on the page", name)
		}
	}
	if e.co.placed != 0 {
		t.Error("an order was placed as a guest while the identity was unknown")
	}
	if len(e.ord.listCalls)+len(e.ord.getCalls) != 0 {
		t.Error("orders were read under the wrong identity")
	}
	// anonymous visitors (no cookie) do not notice
	if rr := e.serve(http.MethodGet, "/orders", "", anon("anon-1"), nil); rr.Code != http.StatusOK {
		t.Errorf("anonymous orders: status %d", rr.Code)
	}
	if rr := e.serve(http.MethodPost, "/cart/checkout", goodOrder, anon("anon-1"), nil); rr.Code != http.StatusOK {
		t.Errorf("anonymous checkout: status %d", rr.Code)
	}
}

// ---------------------------------------------------------------------------
// secrets
// ---------------------------------------------------------------------------

func TestNeitherTokenNorPasswordIsLoggedOrRendered(t *testing.T) {
	e := newAcct(t)
	const pw = "s3cret-pass-phrase-xyz"
	bodies := []string{}
	record := func(rr *httptest.ResponseRecorder) *httptest.ResponseRecorder {
		bodies = append(bodies, rr.Body.String(), fmt.Sprint(rr.Header()["Location"]))
		return rr
	}
	rr := record(e.serve(http.MethodPost, "/register", form("name", "Ada", "email", "ada@example.com", "password", pw), anon("anon-1"), nil))
	token := cookieNamed(rr, cookieAuth).Value
	record(e.serve(http.MethodPost, "/login", loginForm("ada@example.com", pw+"-wrong"), anon("anon-1"), nil))
	record(e.serve(http.MethodPost, "/register", form("name", "Ada", "email", "ada@example.com", "password", pw), anon("anon-1"), nil))
	record(e.serve(http.MethodPost, "/register", form("name", "Ada", "email", "bad", "password", pw), anon("anon-1"), nil))
	for _, p := range []string{"/", "/account", "/orders", "/cart"} {
		record(e.serve(http.MethodGet, p, "", asUser("anon-1", token), nil))
	}
	record(e.serve(http.MethodGet, "/", "", asUser("anon-1", "tok_"+strings.Repeat("7", 38)), nil))
	record(e.serve(http.MethodPost, "/logout", "", asUser("anon-1", token), nil))
	record(e.serve(http.MethodGet, "/logout", "", asUser("anon-1", token), nil))

	for _, secret := range []string{token, pw, "tok_" + strings.Repeat("7", 38)} {
		if strings.Contains(e.logs.String(), secret) {
			t.Errorf("the log contains a secret (%d chars)", len(secret))
		}
		for _, b := range bodies {
			if strings.Contains(b, secret) {
				t.Errorf("a page or redirect contains a secret (%d chars)", len(secret))
			}
		}
	}
	if e.logs.Len() == 0 {
		t.Error("nothing was logged at all, the check proves nothing")
	}
}
