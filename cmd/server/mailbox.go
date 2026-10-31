package main

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pbm "frontend/genproto/mailbox"
	pbo "frontend/genproto/orders"
)

// The mailbox: the confirmation mails the shop "sent" (nothing leaves the demo), kept by
// email-service and shown here. /emails lists the visitor's mails, /emails/{id}?order={order}
// shows one. Like the other backends the service is optional: with EMAIL_SERVICE_ADDR set to an
// empty string the routes answer 404 and no link is shown; when it is down the two routes (and
// only they) answer a friendly 503 page. The mailbox also needs order-service, which says
// which orders belong to the visitor: without it the mailbox is off as well.
//
// The mailbox service does no access control of its own (it is handed order ids): THIS file is
// the guard. A list only asks for order ids order-service returned for the visitor, a detail
// page checks the order with the same guard as /orders/{id} before it asks for the mail body.

const (
	defaultEmailAddr = "email-service:8080"

	// the mailbox pages are the whole point of the visit, but a hung service must still end in a quick 503 page
	mailboxReadTimeout = 1500 * time.Millisecond

	// the link on the order page is a nicety: one short call, silent on any failure
	mailboxLinkTimeout = time.Second

	// ListOrderEmails takes 1-50 ids, and so does ListOrders as a page size
	mailboxMaxOrders = 50

	msgMailboxUnavailable = "Your emails are not available right now. Please try again in a moment."
)

// mailboxEnabled tells the templates whether to show the "Your emails" links (set once in main, like ordersEnabled).
var mailboxEnabled bool

// emailAddrFromEnv returns the email-service address: EMAIL_SERVICE_ADDR if set, the default
// otherwise. A variable that is set but empty switches the mailbox off.
func emailAddrFromEnv() string {
	if v, ok := os.LookupEnv("EMAIL_SERVICE_ADDR"); ok {
		return strings.TrimSpace(v)
	}
	return defaultEmailAddr
}

// mailboxUnavailableError marks "email-service did not give a usable answer"; classifyError
// turns it into the friendly 503 page.
type mailboxUnavailableError struct{ cause error }

func (e *mailboxUnavailableError) Error() string {
	return "mailbox unavailable: " + e.cause.Error()
}
func (e *mailboxUnavailableError) Unwrap() error { return e.cause }

func emailURL(emailID, orderID string) string {
	return baseUrl + "/emails/" + url.PathEscape(emailID) + "?order=" + url.QueryEscape(orderID)
}

// emailRow is one line of the list.
type emailRow struct {
	URL      string
	Subject  string
	To       string
	OrderURL string
	OrderID  string // short
	When     string
	ISO      string
}

func newEmailRow(m *pbm.OrderEmail) emailRow {
	row := emailRow{
		URL: emailURL(m.GetId(), m.GetOrderId()), Subject: m.GetSubject(), To: m.GetTo(),
		OrderURL: orderURL(m.GetOrderId()), OrderID: shortOrderID(m.GetOrderId()),
	}
	if row.Subject == "" {
		row.Subject = "(no subject)"
	}
	row.When, row.ISO = formatTime(m.GetCreatedAt())
	return row
}

// emailView is the data of the detail page.
type emailView struct {
	Subject  string
	To       string
	When     string
	ISO      string
	OrderURL string
	OrderID  string // short
	HTMLBody string // a plain string, on purpose: html/template escapes it for the srcdoc attribute, it is never marked safe
	TextBody string
	ListURL  string
}

// mailboxOff answers the 404 of a mailbox route while the mailbox is off.
func (fe *frontendServer) mailboxOff() bool { return fe.mailbox == nil || fe.orders == nil }

// failMailbox answers a failed email-service call: a NotFound is a 404, anything else is the friendly 503 page.
func (fe *frontendServer) failMailbox(log logrus.FieldLogger, r *http.Request, w http.ResponseWriter, err error, what string) {
	if status.Code(err) == codes.NotFound {
		fe.renderHTTPError(log, r, w, errors.Wrap(err, what), http.StatusNotFound)
		return
	}
	fe.renderHTTPError(log, r, w, &mailboxUnavailableError{cause: errors.Wrap(err, what)}, http.StatusServiceUnavailable)
}

// emailsHandler lists the mails of the visitor's orders (the newest 50), newest first.
func (fe *frontendServer) emailsHandler(w http.ResponseWriter, r *http.Request) {
	log := r.Context().Value(ctxKeyLog{}).(logrus.FieldLogger)
	w.Header().Set("Cache-Control", "no-store") // personal mail: no cache keeps a copy
	if fe.mailboxOff() {
		fe.renderHTTPError(log, r, w, errors.New("the mailbox is not enabled"), http.StatusNotFound)
		return
	}
	if fe.identityUnavailable(log, r, w) {
		return
	}

	// whose orders: the signed-in user's, or the ones of the anonymous session
	req := &pbo.ListOrdersRequest{SessionId: sessionID(r), Page: 1, PageSize: mailboxMaxOrders, PlacedOnly: true}
	owns := func(o *pbo.Order) bool { return o.GetSessionId() == req.SessionId }
	if u := userOf(r); u != nil {
		req = &pbo.ListOrdersRequest{UserId: u.ID, Page: 1, PageSize: mailboxMaxOrders, PlacedOnly: true}
		owns = func(o *pbo.Order) bool { return o.GetUserId() == u.ID }
	}

	var rows []emailRow
	if req.UserId != "" || req.SessionId != "" {
		ctx, cancel := context.WithTimeout(r.Context(), orderReadTimeout)
		resp, err := fe.orders.ListOrders(ctx, req)
		cancel()
		if err != nil {
			fe.failOrders(log, r, w, err, "could not list orders")
			return
		}
		ids := make([]string, 0, len(resp.GetOrders()))
		for _, o := range resp.GetOrders() {
			if owns(o) && o.GetOrderId() != "" {
				ids = append(ids, o.GetOrderId())
			}
		}
		if len(ids) > 0 {
			mails, err := fe.listOrderEmails(r.Context(), mailboxReadTimeout, ids)
			if err != nil {
				fe.failMailbox(log, r, w, err, "could not list emails")
				return
			}
			rows = make([]emailRow, len(mails))
			for i, m := range mails {
				rows[i] = newEmailRow(m)
			}
		}
	}
	log.WithField("emails", len(rows)).Info("emails")

	if err := getTemplates().ExecuteTemplate(w, "emails_list", injectCommonTemplateData(r, map[string]interface{}{
		"title":      "Your emails",
		"categories": fe.navCategories(r.Context()),
		"rows":       rows,
	})); err != nil {
		log.Println(err)
	}
}

// listOrderEmails asks for the mails of the given orders and keeps only those that belong to
// one of them (the answer is trusted no further than the question), newest first as received.
func (fe *frontendServer) listOrderEmails(ctx context.Context, timeout time.Duration, orderIDs []string) ([]*pbm.OrderEmail, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resp, err := fe.mailbox.ListOrderEmails(ctx, &pbm.ListOrderEmailsRequest{OrderIds: orderIDs})
	if err != nil {
		return nil, err
	}
	asked := make(map[string]bool, len(orderIDs))
	for _, id := range orderIDs {
		asked[id] = true
	}
	var out []*pbm.OrderEmail
	for _, m := range resp.GetEmails() {
		if m.GetId() != "" && asked[m.GetOrderId()] {
			out = append(out, m)
		}
	}
	return out, nil
}

// confirmationEmailURL is the link of the order page: the URL of the newest mail of the order, or ""
// when there is none, the mailbox is off, or anything went wrong (one call, 1 s, no error shown).
func (fe *frontendServer) confirmationEmailURL(ctx context.Context, log logrus.FieldLogger, orderID string) string {
	if fe.mailboxOff() {
		return ""
	}
	mails, err := fe.listOrderEmails(ctx, mailboxLinkTimeout, []string{orderID})
	if err != nil {
		log.WithField("error", err).Warn("confirmation email link left out: mailbox unavailable")
		return ""
	}
	if len(mails) == 0 {
		return ""
	}
	return emailURL(mails[0].GetId(), orderID)
}

// emailHandler shows one mail. Ownership comes first: the order in ?order= must be the visitor's
// (the same guard and the same 404 as /orders/{id}); only then the mail is fetched, and it must
// belong to that order. Everything that is not the visitor's mail gives the same 404 page.
func (fe *frontendServer) emailHandler(w http.ResponseWriter, r *http.Request) {
	log := r.Context().Value(ctxKeyLog{}).(logrus.FieldLogger)
	w.Header().Set("Cache-Control", "no-store")
	if fe.mailboxOff() {
		fe.renderHTTPError(log, r, w, errors.New("the mailbox is not enabled"), http.StatusNotFound)
		return
	}
	id, orderID := mux.Vars(r)["id"], r.URL.Query().Get("order")
	if !orderIDPattern.MatchString(id) || !orderIDPattern.MatchString(orderID) {
		fe.renderHTTPError(log, r, w, errors.New("malformed email or order id"), http.StatusNotFound)
		return
	}
	if fe.identityUnavailable(log, r, w) {
		return
	}
	if fe.ownedOrder(log, w, r, orderID) == nil {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), mailboxReadTimeout)
	defer cancel()
	m, err := fe.mailbox.GetOrderEmail(ctx, &pbm.GetOrderEmailRequest{EmailId: id, OrderId: orderID})
	if err != nil {
		fe.failMailbox(log, r, w, err, "could not get email")
		return
	}
	if m.GetId() != id || m.GetOrderId() != orderID {
		fe.renderHTTPError(log, r, w, errors.New("email belongs to another order"), http.StatusNotFound)
		return
	}
	view := emailView{
		Subject: m.GetSubject(), To: m.GetTo(), OrderURL: orderURL(orderID), OrderID: shortOrderID(orderID),
		HTMLBody: m.GetHtmlBody(), TextBody: m.GetTextBody(), ListURL: baseUrl + "/emails",
	}
	if view.Subject == "" {
		view.Subject = "(no subject)"
	}
	view.When, view.ISO = formatTime(m.GetCreatedAt())
	log.WithField("order", orderID).WithField("email", id).Info("email")

	if err := getTemplates().ExecuteTemplate(w, "email_detail", injectCommonTemplateData(r, map[string]interface{}{
		"title":      view.Subject,
		"categories": fe.navCategories(r.Context()),
		"m":          view,
	})); err != nil {
		log.Println(err)
	}
}
