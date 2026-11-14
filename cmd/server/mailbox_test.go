package main

import (
	"bytes"
	"context"
	"html"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
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

	pbm "frontend/genproto/mailbox"
	pbo "frontend/genproto/orders"
)

// ---------------------------------------------------------------------------
// fake mailbox client (implements pbm.MailboxServiceClient, no network)
// ---------------------------------------------------------------------------

type fakeMailbox struct {
	mu        sync.Mutex
	emails    []*pbm.OrderEmail // newest first, WITH bodies
	listCalls []*pbm.ListOrderEmailsRequest
	listTTL   []time.Duration // the deadline each List call came with (0 = none)
	getCalls  []*pbm.GetOrderEmailRequest
	err       error // returned by every call when set
	block     bool  // wait for the caller's deadline, like a hung service
	getHook   func(*pbm.OrderEmail) *pbm.OrderEmail
}

func (f *fakeMailbox) wait(ctx context.Context) error {
	if f.block {
		<-ctx.Done()
		return status.FromContextError(ctx.Err()).Err()
	}
	return f.err
}

// ListOrderEmails behaves like email-service: the mails of the asked orders, without bodies.
func (f *fakeMailbox) ListOrderEmails(ctx context.Context, in *pbm.ListOrderEmailsRequest, _ ...grpc.CallOption) (*pbm.ListOrderEmailsResponse, error) {
	f.mu.Lock()
	f.listCalls = append(f.listCalls, in)
	var ttl time.Duration
	if d, ok := ctx.Deadline(); ok {
		ttl = time.Until(d)
	}
	f.listTTL = append(f.listTTL, ttl)
	f.mu.Unlock()
	if err := f.wait(ctx); err != nil {
		return nil, err
	}
	asked := map[string]bool{}
	for _, id := range in.OrderIds {
		asked[id] = true
	}
	out := &pbm.ListOrderEmailsResponse{}
	for _, m := range f.emails {
		if asked[m.OrderId] {
			out.Emails = append(out.Emails, &pbm.OrderEmail{Id: m.Id, OrderId: m.OrderId, To: m.To, Subject: m.Subject, CreatedAt: m.CreatedAt})
		}
	}
	return out, nil
}

// GetOrderEmail behaves like email-service: the mail must belong to the order, else NOT_FOUND.
func (f *fakeMailbox) GetOrderEmail(ctx context.Context, in *pbm.GetOrderEmailRequest, _ ...grpc.CallOption) (*pbm.OrderEmail, error) {
	f.mu.Lock()
	f.getCalls = append(f.getCalls, in)
	f.mu.Unlock()
	if err := f.wait(ctx); err != nil {
		return nil, err
	}
	for _, m := range f.emails {
		if m.Id == in.EmailId && m.OrderId == in.OrderId {
			if f.getHook != nil {
				return f.getHook(m), nil
			}
			return m, nil
		}
	}
	return nil, status.Error(codes.NotFound, "email not found")
}

func sampleEmail(id, orderID string) *pbm.OrderEmail {
	return &pbm.OrderEmail{
		Id: id, OrderId: orderID, To: "ada.secret@example.com", Subject: "Your Everly order " + orderID,
		HtmlBody:  "<html><body><h1>Thank you!</h1><p>BODY-OF-" + id + "</p></body></html>",
		TextBody:  "Thank you! TEXT-OF-" + id,
		CreatedAt: timestamppb.New(evalAt),
	}
}

// ---------------------------------------------------------------------------
// harness: the real middleware chain of main.go over fake backends
// ---------------------------------------------------------------------------

type mailEnv struct {
	fe    *frontendServer
	users *fakeUsers
	ord   *fakeOrders
	mb    *fakeMailbox
	logs  *bytes.Buffer
	h     http.Handler
}

func setMailboxEnabled(t *testing.T, v bool) {
	t.Helper()
	old := mailboxEnabled
	mailboxEnabled = v
	t.Cleanup(func() { mailboxEnabled = old })
}

// newMailEnv builds a frontend with orders, accounts and the mailbox ON; tests switch parts off.
func newMailEnv(t *testing.T) *mailEnv {
	t.Helper()
	setAccountsEnabled(t, true)
	setOrdersEnabled(t, true)
	setMailboxEnabled(t, true)
	e := &mailEnv{users: newFakeUsers(), ord: &fakeOrders{}, mb: &fakeMailbox{}, logs: &bytes.Buffer{}}
	e.fe = newFakeFrontend(t, sampleCatalog())
	e.fe.users, e.fe.orders, e.fe.mailbox = e.users, e.ord, e.mb

	r := mux.NewRouter()
	r.HandleFunc("/", e.fe.homeHandler).Methods(http.MethodGet)
	r.HandleFunc("/orders", e.fe.ordersHistoryHandler).Methods(http.MethodGet, http.MethodHead)
	r.HandleFunc("/orders/{id}", e.fe.orderTrackingHandler).Methods(http.MethodGet, http.MethodHead)
	r.HandleFunc("/emails", e.fe.emailsHandler).Methods(http.MethodGet, http.MethodHead)
	r.HandleFunc("/emails/{id}", e.fe.emailHandler).Methods(http.MethodGet, http.MethodHead)
	e.fe.accountRoutes(r)

	log := logrus.New()
	log.SetOutput(e.logs)
	log.SetLevel(logrus.DebugLevel)
	e.h = ensureSessionID(&logHandler{log: log, next: e.fe.withUser(r)})
	return e
}

func (e *mailEnv) get(target string, cookies []*http.Cookie, header map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
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

// seed gives two visitors one order and one mail each: o-1/e-1 of session s1, o-2/e-2 of session s2.
func (e *mailEnv) seed() {
	e.ord.orders = []*pbo.Order{
		sampleOrder("o-1", "s1", stPlaced, time.Minute),
		sampleOrder("o-2", "s2", stPlaced, time.Minute),
	}
	e.mb.emails = []*pbm.OrderEmail{sampleEmail("e-1", "o-1"), sampleEmail("e-2", "o-2")}
}

func assertNotContains(t *testing.T, html string, nots ...string) {
	t.Helper()
	for _, n := range nots {
		if strings.Contains(html, n) {
			t.Errorf("rendered page must not contain %q", n)
		}
	}
}

func assertNoStore(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

// ---------------------------------------------------------------------------
// list
// ---------------------------------------------------------------------------

func TestEmailsList_AnonymousSessionOrders(t *testing.T) {
	e := newMailEnv(t)
	e.seed()
	rr := e.get("/emails", anon("s1"), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	assertNoStore(t, rr)
	page := rr.Body.String()
	assertContains(t, page, "<title>Your emails | "+brand.Name, "1 email, newest first", "Your Everly order o-1",
		"ada.secret@example.com", `href="/emails/e-1?order=o-1"`, `href="/orders/o-1"`, "#o-1", `datetime="2031-03-04T10:30:00Z"`)
	// somebody else's mail (match the links, not bare substrings: the footer prints random UUIDs that can contain "e-2"), and no bodies in a list
	assertNotContains(t, page, "/emails/e-2", "order=o-2", "/orders/o-2", "#o-2", "BODY-OF")

	// orders by session, placed only, first page of 50
	if len(e.ord.listCalls) != 1 {
		t.Fatalf("want one ListOrders call, got %d", len(e.ord.listCalls))
	}
	c := e.ord.listCalls[0]
	if c.SessionId != "s1" || c.UserId != "" || !c.PlacedOnly || c.Page != 1 || c.PageSize != 50 {
		t.Errorf("ListOrders request %v, want session s1, placed only, page 1, size 50", c)
	}
	// only the visitor's order ids go to the mailbox
	if len(e.mb.listCalls) != 1 || strings.Join(e.mb.listCalls[0].OrderIds, ",") != "o-1" {
		t.Errorf("ListOrderEmails calls %v, want exactly [o-1]", e.mb.listCalls)
	}
	if len(e.mb.getCalls) != 0 {
		t.Error("a list must not fetch bodies")
	}
}

func TestEmailsList_SignedInUserOrders(t *testing.T) {
	e := newMailEnv(t)
	u, token := e.users.signIn("ada@example.com", "Ada", "correct horse battery")
	// two orders of the user, placed from two different sessions, one of somebody else
	e.ord.orders = []*pbo.Order{sampleOrder("o-a", "s1", stPlaced, time.Minute), sampleOrder("o-b", "s9", stShipped, time.Hour), sampleOrder("o-x", "s1", stPlaced, time.Minute)}
	e.ord.orders[0].UserId, e.ord.orders[1].UserId = u.id, u.id
	e.mb.emails = []*pbm.OrderEmail{sampleEmail("e-a", "o-a"), sampleEmail("e-b", "o-b"), sampleEmail("e-x", "o-x")}

	rr := e.get("/emails", asUser("s1", token), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	assertContains(t, rr.Body.String(), "2 emails, newest first", `/emails/e-a?order=o-a`, `/emails/e-b?order=o-b`)
	assertNotContains(t, rr.Body.String(), "e-x", "o-x")
	c := e.ord.listCalls[0]
	if c.UserId != u.id || c.SessionId != "" || !c.PlacedOnly || c.PageSize != 50 {
		t.Errorf("ListOrders request %v, want the user id and no session id", c)
	}
	if got := e.mb.listCalls[0].OrderIds; len(got) != 2 {
		t.Errorf("ListOrderEmails ids %v, want the user's two orders", got)
	}
}

func TestEmailsList_EmptyStates(t *testing.T) {
	e := newMailEnv(t)
	// no orders at all: nothing to ask the mailbox
	rr := e.get("/emails", anon("s1"), nil)
	assertContains(t, rr.Body.String(), "No emails yet", `href="/">Start shopping`)
	assertNotContains(t, rr.Body.String(), "mail-card")
	if len(e.mb.listCalls) != 0 {
		t.Error("no orders: the mailbox must not be asked")
	}
	// orders without mail (the consumer is behind): same text
	e.ord.orders = []*pbo.Order{sampleOrder("o-1", "s1", stPlaced, time.Minute)}
	rr = e.get("/emails", anon("s1"), nil)
	assertContains(t, rr.Body.String(), "No emails yet")
	if len(e.mb.listCalls) != 1 {
		t.Error("with orders the mailbox is asked once")
	}
}

func TestEmailsList_MailOfAnotherOrderInTheAnswerIsDropped(t *testing.T) {
	e := newMailEnv(t)
	e.seed()
	// a misbehaving service answers with a mail of an order that was not asked for
	e.fe.mailbox = &leakyMailbox{fakeMailbox: e.mb}
	rr := e.get("/emails", anon("s1"), nil)
	assertNotContains(t, rr.Body.String(), "/emails/e-2", "order=o-2", "/orders/o-2")
	assertContains(t, rr.Body.String(), "e-1")
}

type leakyMailbox struct{ *fakeMailbox }

func (l *leakyMailbox) ListOrderEmails(ctx context.Context, in *pbm.ListOrderEmailsRequest, o ...grpc.CallOption) (*pbm.ListOrderEmailsResponse, error) {
	resp, err := l.fakeMailbox.ListOrderEmails(ctx, in, o...)
	if err == nil {
		resp.Emails = append(resp.Emails, sampleEmail("e-2", "o-2"))
	}
	return resp, err
}

// ---------------------------------------------------------------------------
// detail
// ---------------------------------------------------------------------------

func TestEmailDetail_ShowsHeadersFrameAndPlainText(t *testing.T) {
	e := newMailEnv(t)
	e.seed()
	rr := e.get("/emails/e-1?order=o-1", anon("s1"), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %.200s", rr.Code, rr.Body.String())
	}
	assertNoStore(t, rr)
	page := rr.Body.String()
	assertContains(t, page, "<title>Your Everly order o-1 | "+brand.Name, "ada.secret@example.com", `href="/orders/o-1"`, `href="/emails"`,
		`datetime="2031-03-04T10:30:00Z"`, "Plain text version", "Thank you! TEXT-OF-e-1", `<iframe class="mail-frame"`)
	// ownership first, then exactly one GetOrderEmail with both ids
	if len(e.ord.getCalls) != 1 || e.ord.getCalls[0].SessionId != "s1" || e.ord.getCalls[0].OrderId != "o-1" {
		t.Errorf("GetOrder calls %v, want the session guard for o-1", e.ord.getCalls)
	}
	if len(e.mb.getCalls) != 1 || e.mb.getCalls[0].EmailId != "e-1" || e.mb.getCalls[0].OrderId != "o-1" {
		t.Errorf("GetOrderEmail calls %v, want (e-1, o-1)", e.mb.getCalls)
	}
}

func TestEmailDetail_SignedInUserGuard(t *testing.T) {
	e := newMailEnv(t)
	u, token := e.users.signIn("ada@example.com", "Ada", "correct horse battery")
	_, tokenB := e.users.signIn("bob@example.com", "Bob", "correct horse battery")
	e.ord.orders = []*pbo.Order{sampleOrder("o-a", "s9", stPlaced, time.Minute)}
	e.ord.orders[0].UserId = u.id
	e.mb.emails = []*pbm.OrderEmail{sampleEmail("e-a", "o-a")}

	// the owner reads it from any session
	if rr := e.get("/emails/e-a?order=o-a", asUser("s1", token), nil); rr.Code != http.StatusOK {
		t.Fatalf("owner: status %d", rr.Code)
	}
	if e.ord.getCalls[0].UserId != u.id || e.ord.getCalls[0].SessionId != "" {
		t.Errorf("GetOrder guard %v, want the user id", e.ord.getCalls[0])
	}
	// another user, and an anonymous visitor with another session: 404, no body fetched
	e.mb.getCalls = nil
	for name, cookies := range map[string][]*http.Cookie{"other user": asUser("s1", tokenB), "anonymous, other session": anon("s1")} {
		rr := e.get("/emails/e-a?order=o-a", cookies, nil)
		if rr.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", name, rr.Code)
		}
		assertNotContains(t, rr.Body.String(), "BODY-OF", "TEXT-OF", "ada.secret")
	}
	if len(e.mb.getCalls) != 0 {
		t.Errorf("GetOrderEmail was called %d times for a visitor who does not own the order", len(e.mb.getCalls))
	}
}

func TestEmailDetail_OwnershipAnotherVisitorGets404AndNoBodyIsFetched(t *testing.T) {
	e := newMailEnv(t)
	e.seed()
	foreign := e.get("/emails/e-2?order=o-2", anon("s1"), nil) // s2's mail, asked by s1
	unknown := e.get("/emails/e-nope?order=o-nope", anon("s1"), nil)
	if foreign.Code != http.StatusNotFound || unknown.Code != http.StatusNotFound {
		t.Fatalf("foreign %d, unknown %d, want 404 for both", foreign.Code, unknown.Code)
	}
	reqID := regexp.MustCompile(`request-id: \S+`)
	if reqID.ReplaceAllString(foreign.Body.String(), "") != reqID.ReplaceAllString(unknown.Body.String(), "") {
		t.Error("somebody else's mail must look exactly like an unknown one")
	}
	assertNoStore(t, foreign)
	assertNotContains(t, foreign.Body.String(), "BODY-OF", "TEXT-OF", "ada.secret")
	if len(e.mb.getCalls) != 0 {
		t.Errorf("GetOrderEmail was called %d times, want 0", len(e.mb.getCalls))
	}
	// no session cookie value at all
	e.mb.getCalls = nil
	if rr := e.get("/emails/e-2?order=o-2", nil, nil); rr.Code != http.StatusNotFound || len(e.mb.getCalls) != 0 {
		t.Errorf("a new visitor: status %d, %d body fetches", rr.Code, len(e.mb.getCalls))
	}
}

func TestEmailDetail_TheOrderInTheQueryMustBelongToTheVisitor(t *testing.T) {
	e := newMailEnv(t)
	e.seed()
	// s1 owns o-1; e-2 is the mail of o-2. Pairing e-2 with the visitor's own o-1 must not work:
	// the service says NOT_FOUND (mail does not belong to the order) -> 404, nothing shown
	rr := e.get("/emails/e-2?order=o-1", anon("s1"), nil)
	if rr.Code != http.StatusNotFound {
		t.Errorf("another order's mail under my order: status %d, want 404", rr.Code)
	}
	assertNotContains(t, rr.Body.String(), "BODY-OF", "TEXT-OF")
	// ... and a service that wrongly hands the mail out is not trusted: the frontend checks the order of the answer
	e.mb.getHook = func(*pbm.OrderEmail) *pbm.OrderEmail { return sampleEmail("e-2", "o-2") }
	rr = e.get("/emails/e-2?order=o-1", anon("s1"), nil)
	if rr.Code != http.StatusNotFound {
		t.Errorf("service answered a mail of another order: status %d, want 404", rr.Code)
	}
	assertNotContains(t, rr.Body.String(), "BODY-OF", "TEXT-OF")
	// a missing or malformed ?order= is a 404 without any call
	e.mb.getHook, e.mb.getCalls, e.ord.getCalls = nil, nil, nil
	for _, target := range []string{"/emails/e-1", "/emails/e-1?order=", "/emails/e-1?order=../x", "/emails/e-1?order=" + strings.Repeat("a", 65), "/emails/-x?order=o-1"} {
		if rr := e.get(target, anon("s1"), nil); rr.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", target, rr.Code)
		}
	}
	if len(e.mb.getCalls) != 0 || len(e.ord.getCalls) != 0 {
		t.Error("malformed ids must be refused before any backend call")
	}
}

func TestEmailDetail_SubjectToTheSameIdentityGateAsOrders(t *testing.T) {
	e := newMailEnv(t)
	e.seed()
	e.users.err = status.Error(codes.Unavailable, "down")
	_, tokenLike := "", "tok_"+strings.Repeat("0", 38)
	for _, target := range []string{"/emails", "/emails/e-1?order=o-1"} {
		rr := e.get(target, asUser("s1", tokenLike), nil)
		if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), "Accounts are not available") {
			t.Errorf("%s: status %d, want the accounts 503 page", target, rr.Code)
		}
	}
	if len(e.mb.listCalls)+len(e.mb.getCalls) != 0 {
		t.Error("an unresolved identity must not reach the mailbox")
	}
}

// ---------------------------------------------------------------------------
// the iframe
// ---------------------------------------------------------------------------

var (
	iframeRe = regexp.MustCompile(`<iframe\b[^>]*>`)
	srcdocRe = regexp.MustCompile(`\ssrcdoc="([^"]*)"`)
)

func TestEmailDetail_IframeIsSandboxedAndTheBodyEscaped(t *testing.T) {
	hostile := `<p>hi</p>" onload="alert(1)" x="<script>alert('xss')</script><img src=x onerror=alert(2)>&quot;</iframe><script>alert(3)</script>'&amp;`
	e := newMailEnv(t)
	e.seed()
	e.mb.emails[0].HtmlBody = hostile
	e.mb.emails[0].TextBody = `plain <script>alert(4)</script> "q" &quot;`
	e.mb.emails[0].Subject = `<script>alert(5)</script> "subject"`
	rr := e.get("/emails/e-1?order=o-1", anon("s1"), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	page := rr.Body.String()

	frames := iframeRe.FindAllString(page, -1)
	if len(frames) != 1 {
		t.Fatalf("want exactly one iframe, got %d", len(frames))
	}
	frame := frames[0]
	// `sandbox` as a bare attribute: empty value, which is the strictest sandbox
	if !regexp.MustCompile(`\ssandbox[\s>]`).MatchString(frame) || strings.Contains(frame, "sandbox=") {
		t.Errorf("iframe must carry an empty sandbox attribute: %s", frame)
	}
	attrs := srcdocRe.ReplaceAllString(frame, "") // the attributes besides srcdoc
	for _, bad := range []string{"allow-", "allow=", "allowfullscreen", " src="} {
		if strings.Contains(attrs, bad) {
			t.Errorf("iframe must not have %q: %s", bad, attrs)
		}
	}
	// the attribute value is one HTML-escaped string; the browser decodes it ONCE and gets the mail verbatim
	m := srcdocRe.FindStringSubmatch(frame)
	if m == nil {
		t.Fatalf("no srcdoc=\"...\" in %s", frame)
	}
	if got := html.UnescapeString(m[1]); got != hostile {
		t.Errorf("srcdoc decodes to %q, want the body verbatim %q", got, hostile)
	}
	// and nothing of it is live in the page around it: quotes and angle brackets inside the value are entities
	if strings.ContainsAny(m[1], `<>"`) {
		t.Errorf("srcdoc value contains a raw quote or angle bracket: %s", m[1])
	}
	assertNotContains(t, page, "<script>alert", "<img src=x", `onload="alert`, "</iframe><script>")
	// subject and text are escaped in the page like any text
	assertContains(t, page, "&lt;script&gt;alert(4)&lt;/script&gt;", "&lt;script&gt;alert(5)&lt;/script&gt;")
}

func TestEmailDetail_NoInlineScriptAndNoHTMLBodyCase(t *testing.T) {
	e := newMailEnv(t)
	e.seed()
	rr := e.get("/emails/e-1?order=o-1", anon("s1"), nil)
	page := rr.Body.String()
	main := page[strings.Index(page, `<main id="main"`):strings.Index(page, "</main>")]
	assertNotContains(t, main, "<script", " onclick=", " onload=", "javascript:", "style=")

	// a mail without HTML part: no iframe, the text is shown open
	e.mb.emails[0].HtmlBody = ""
	page = e.get("/emails/e-1?order=o-1", anon("s1"), nil).Body.String()
	assertNotContains(t, page, "<iframe")
	assertContains(t, page, "<details", "TEXT-OF-e-1")
	if !regexp.MustCompile(`<details[^>]* open>`).MatchString(page) {
		t.Error("with no HTML part the plain text is open")
	}
}

func TestEmailDetail_NoStoreOnEveryAnswer(t *testing.T) {
	e := newMailEnv(t)
	e.seed()
	for _, target := range []string{"/emails", "/emails/e-1?order=o-1", "/emails/e-2?order=o-2", "/emails/e-1", "/emails/e-1?order=o-nope"} {
		assertNoStore(t, e.get(target, anon("s1"), nil))
	}
	e.mb.err = status.Error(codes.Unavailable, "down")
	assertNoStore(t, e.get("/emails", anon("s1"), nil))
	e.fe.mailbox = nil
	assertNoStore(t, e.get("/emails", anon("s1"), nil))
}

// ---------------------------------------------------------------------------
// off and down
// ---------------------------------------------------------------------------

func TestMailbox_OffRoutes404LinksHidden(t *testing.T) {
	e := newMailEnv(t)
	e.seed()
	e.fe.mailbox = nil
	setMailboxEnabled(t, false)
	for _, target := range []string{"/emails", "/emails/e-1?order=o-1"} {
		if rr := e.get(target, anon("s1"), nil); rr.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", target, rr.Code)
		}
	}
	u, token := e.users.signIn("ada@example.com", "Ada", "correct horse battery")
	e.ord.orders[0].UserId = u.id
	for name, rr := range map[string]*httptest.ResponseRecorder{
		"home":    e.get("/", anon("s1"), nil),
		"orders":  e.get("/orders", anon("s1"), nil),
		"order":   e.get("/orders/o-1", anon("s1"), nil),
		"account": e.get("/account", asUser("s1", token), nil),
	} {
		if rr.Code != http.StatusOK {
			t.Errorf("%s: status %d", name, rr.Code)
		}
		assertNotContains(t, rr.Body.String(), "/emails", "Your emails", "View confirmation email")
	}
}

func TestMailbox_NeedsTheOrdersToo(t *testing.T) {
	e := newMailEnv(t)
	e.seed()
	e.fe.orders = nil // ORDER_SERVICE_ADDR empty: main leaves the mailbox off, and the handlers agree
	for _, target := range []string{"/emails", "/emails/e-1?order=o-1"} {
		if rr := e.get(target, anon("s1"), nil); rr.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404 without order-service", target, rr.Code)
		}
	}
}

func TestMailbox_LinksShownWhenOn(t *testing.T) {
	e := newMailEnv(t)
	e.seed()
	u, token := e.users.signIn("ada@example.com", "Ada", "correct horse battery")
	e.ord.orders[0].UserId = u.id
	for name, rr := range map[string]*httptest.ResponseRecorder{
		"home (footer)": e.get("/", anon("s1"), nil),
		"orders":        e.get("/orders", asUser("s1", token), nil),
		"account":       e.get("/account", asUser("s1", token), nil),
	} {
		if rr.Code != http.StatusOK {
			t.Errorf("%s: status %d", name, rr.Code)
		}
		assertContains(t, rr.Body.String(), `href="/emails"`, "Your emails", "/static/styles/mailbox.css")
	}
	// footer on the mailbox pages themselves
	assertContains(t, e.get("/emails", anon("s1"), nil).Body.String(), `<li><a href="/emails">Your emails</a></li>`)
}

func TestMailbox_DownGives503OnlyOnTheMailboxRoutes(t *testing.T) {
	e := newMailEnv(t)
	e.seed()
	u, token := e.users.signIn("ada@example.com", "Ada", "correct horse battery")
	e.ord.orders[0].UserId = u.id
	for name, err := range map[string]error{
		"unavailable": status.Error(codes.Unavailable, "dial tcp 10.1.2.3:8080: connect: connection refused"),
		"deadline":    status.Error(codes.DeadlineExceeded, "slow"),
		"internal":    status.Error(codes.Internal, "sqlite: disk I/O error /data/mailbox.db"),
	} {
		e.mb.err = err
		for _, target := range []string{"/emails", "/emails/e-1?order=o-1"} {
			rr := e.get(target, anon("s1"), nil)
			if rr.Code != http.StatusServiceUnavailable {
				t.Errorf("%s %s: status %d, want 503", name, target, rr.Code)
			}
			assertContains(t, rr.Body.String(), "Your emails are not available")
			assertNotContains(t, rr.Body.String(), "10.1.2.3", "sqlite", "mailbox.db", "BODY-OF")
			assertNoStore(t, rr)
		}
		// everything else works, and the order page just has no link
		for _, target := range []string{"/", "/orders", "/orders/o-1"} {
			rr := e.get(target, anon("s1"), nil)
			if rr.Code != http.StatusOK {
				t.Errorf("%s with the mailbox %s: status %d, want 200", target, name, rr.Code)
			}
			assertNotContains(t, rr.Body.String(), "View confirmation email")
		}
		if rr := e.get("/account", asUser("s1", token), nil); rr.Code != http.StatusOK {
			t.Errorf("account with the mailbox %s: status %d", name, rr.Code)
		}
	}
}

func TestMailbox_HungServiceEndsInA503Quickly(t *testing.T) {
	e := newMailEnv(t)
	e.seed()
	e.mb.block = true
	start := time.Now()
	rr := e.get("/emails", anon("s1"), nil)
	if rr.Code != http.StatusServiceUnavailable || time.Since(start) > 4*time.Second {
		t.Errorf("hung mailbox: status %d after %v, want a quick 503", rr.Code, time.Since(start))
	}
	// the order page waits at most about a second for the link, then renders without it
	start = time.Now()
	rr = e.get("/orders/o-1", anon("s1"), nil)
	if rr.Code != http.StatusOK || time.Since(start) > 3*time.Second {
		t.Errorf("order page with a hung mailbox: status %d after %v", rr.Code, time.Since(start))
	}
	assertNotContains(t, rr.Body.String(), "View confirmation email")
}

func TestMailbox_OrderServiceDownGives503OnTheMailboxRoutes(t *testing.T) {
	e := newMailEnv(t)
	e.seed()
	e.ord.err = status.Error(codes.Unavailable, "down")
	for _, target := range []string{"/emails", "/emails/e-1?order=o-1"} {
		if rr := e.get(target, anon("s1"), nil); rr.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: status %d, want 503", target, rr.Code)
		}
	}
	if len(e.mb.getCalls) != 0 {
		t.Error("the body must not be fetched before ownership is known")
	}
}

func TestEmailAddrFromEnv(t *testing.T) {
	t.Setenv("EMAIL_SERVICE_ADDR", "")
	if err := os.Unsetenv("EMAIL_SERVICE_ADDR"); err != nil {
		t.Fatal(err)
	}
	if got := emailAddrFromEnv(); got != "email-service:8080" {
		t.Errorf("default %q, want email-service:8080", got)
	}
	t.Setenv("EMAIL_SERVICE_ADDR", "")
	if got := emailAddrFromEnv(); got != "" {
		t.Errorf("empty variable gave %q, want \"\" (mailbox off)", got)
	}
	t.Setenv("EMAIL_SERVICE_ADDR", " mail:9 ")
	if got := emailAddrFromEnv(); got != "mail:9" {
		t.Errorf("got %q, want the trimmed address", got)
	}
}

// ---------------------------------------------------------------------------
// the link on the order page
// ---------------------------------------------------------------------------

func TestOrderPage_ConfirmationEmailLink(t *testing.T) {
	e := newMailEnv(t)
	e.seed()
	rr := e.get("/orders/o-1", anon("s1"), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	assertContains(t, rr.Body.String(), `href="/emails/e-1?order=o-1">View confirmation email`)
	if len(e.mb.listCalls) != 1 || strings.Join(e.mb.listCalls[0].OrderIds, ",") != "o-1" {
		t.Errorf("ListOrderEmails calls %v, want one call for o-1", e.mb.listCalls)
	}
	if ttl := e.mb.listTTL[0]; ttl <= 0 || ttl > time.Second {
		t.Errorf("the link call came with a deadline of %v, want 1 s at most", ttl)
	}
	if len(e.mb.getCalls) != 0 {
		t.Error("the order page must not fetch a body")
	}

	// no mail for the order: no link
	e.mb.emails = nil
	assertNotContains(t, e.get("/orders/o-1", anon("s1"), nil).Body.String(), "View confirmation email")

	// the htmx poll of an active order does not ask the mailbox again
	e.mb.emails = []*pbm.OrderEmail{sampleEmail("e-1", "o-1")}
	e.mb.listCalls = nil
	frag := e.get("/orders/o-1", anon("s1"), map[string]string{"HX-Request": "true"})
	if frag.Code != http.StatusOK && frag.Code != 286 {
		t.Fatalf("fragment status %d", frag.Code)
	}
	if len(e.mb.listCalls) != 0 {
		t.Error("a poll must not call the mailbox")
	}
}

func TestOrderPage_LinkOfTheNewestMail(t *testing.T) {
	e := newMailEnv(t)
	e.seed()
	e.mb.emails = []*pbm.OrderEmail{sampleEmail("e-new", "o-1"), sampleEmail("e-1", "o-1")} // newest first
	assertContains(t, e.get("/orders/o-1", anon("s1"), nil).Body.String(), `href="/emails/e-new?order=o-1"`)
}

func TestOrderPage_MailboxErrorIsSilent(t *testing.T) {
	e := newMailEnv(t)
	e.seed()
	e.mb.err = status.Error(codes.Unavailable, "down")
	rr := e.get("/orders/o-1", anon("s1"), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rr.Code)
	}
	assertContains(t, rr.Body.String(), "Order status", "Shipping to")
	assertNotContains(t, rr.Body.String(), "View confirmation email", "not available")
	if !strings.Contains(e.logs.String(), "confirmation email link left out") {
		t.Error("the failure should be logged (warn), not shown")
	}
}

// ---------------------------------------------------------------------------
// CSS: mobile layout and reuse of the design system
// ---------------------------------------------------------------------------

func TestMailboxCSS_MobileLayoutAndTokens(t *testing.T) {
	css, err := os.ReadFile("../../static/styles/mailbox.css")
	if err != nil {
		t.Fatal(err)
	}
	s := string(css)
	for _, want := range []string{"@media (max-width: 479px)", "@media (min-width: 720px)", ".mail-frame", "min-height", "overflow-wrap", "var(--color-border)", "var(--color-surface)"} {
		if !strings.Contains(s, want) {
			t.Errorf("mailbox.css lacks %q", want)
		}
	}
	// colours come from the tokens (light/dark follow along); only the mail's own white page is literal
	if hex := regexp.MustCompile(`#[0-9a-fA-F]{3,6}\b`).FindAllString(strings.ReplaceAll(s, "background: #fff;", ""), -1); len(hex) != 0 {
		t.Errorf("mailbox.css uses literal colours %v, use the tokens", hex)
	}
}
