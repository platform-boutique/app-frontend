package main

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "frontend/genproto"
)

// Error pages. Backend gRPC status codes are passed through by the services and
// mapped here, so a visitor sees a fitting status and a readable sentence, never a
// stack trace or a backend error text (those go to the server log only):
//
//	InvalidArgument, FailedPrecondition -> 422, the backend's message (it is meant for people);
//	                                       "out of stock: ..." from checkout gets a link back to the cart
//	Aborted                             -> 409, "your order is being processed" (the same order is already in flight)
//	NotFound                            -> 404
//	Unavailable, DeadlineExceeded       -> 503
//	anything else                       -> 500, generic text

type errorPage struct {
	Code    int
	Title   string
	Message string
}

const (
	msgServerError   = "Something went wrong on our side. Please try again in a moment."
	msgUnavailable   = "Part of the shop is busy right now. Please try again in a moment."
	msgNotFound      = "We could not find what you were looking for. It may have been removed."
	msgBadRequest    = "That request could not be understood."
	msgForbidden     = "That request was refused."
	msgUnprocessable = "We could not process that request. Please check your input and try again."

	titleOrderInProgress = "Your order is being processed"
	msgOrderInProgress   = "Your order is being processed, please do not place it again. Check Orders in a minute to see whether it went through."
	msgOrderInProgressNo = "Your order is being processed, please do not place it again. Give it a minute before you look again."

	// prefixes of the FailedPrecondition messages of checkout that send the visitor back to the cart
	outOfStockPrefix  = "out of stock:"
	attemptFailedText = "this attempt failed"
)

// rpcWrapper is the "rpc error: code = X desc = " prefix a status error gets when it is printed.
var rpcWrapper = regexp.MustCompile(`^(?:rpc error: code = \w+ desc = )+`)

// nestedDecline finds a payment rejection in the text of an Internal error. Until
// checkout-service passes the payment status through, it answers Internal with
// "failed to charge card: ...: rpc error: code = InvalidArgument desc = <reason>: <text>".
var nestedDecline = regexp.MustCompile(`rpc error: code = (?:InvalidArgument|FailedPrecondition) desc = (.+)$`)

// paymentReason is the machine-readable "card_declined: " prefix of a payment message.
var paymentReason = regexp.MustCompile(`^[a-z_]+: `)

func titleFor(code int) string {
	switch code {
	case http.StatusBadRequest:
		return "Bad request"
	case http.StatusForbidden:
		return "Not allowed"
	case http.StatusNotFound:
		return "Not found"
	case http.StatusUnprocessableEntity:
		return "We could not process that"
	case http.StatusServiceUnavailable:
		return "Temporarily unavailable"
	}
	return "Something went wrong"
}

// readableMessage turns a gRPC description into a sentence for a visitor.
func readableMessage(msg string) string {
	msg = strings.TrimSpace(rpcWrapper.ReplaceAllString(strings.TrimSpace(msg), ""))
	msg = strings.TrimSpace(paymentReason.ReplaceAllString(msg, ""))
	if msg == "" {
		return msgUnprocessable
	}
	return strings.ToUpper(msg[:1]) + msg[1:]
}

// classifyError decides status, title and text of the error page. code is what the
// caller would use for an error that carries no gRPC status.
func classifyError(err error, code int) errorPage {
	var ou *ordersUnavailableError
	if errors.As(err, &ou) {
		return errorPage{http.StatusServiceUnavailable, "Orders are not available", msgOrdersUnavailable}
	}
	var uu *usersUnavailableError
	if errors.As(err, &uu) {
		return errorPage{http.StatusServiceUnavailable, "Accounts are not available", msgAccountsUnavailable}
	}
	var gs interface{ GRPCStatus() *status.Status }
	if errors.As(err, &gs) {
		st := gs.GRPCStatus()
		switch st.Code() {
		case codes.Aborted:
			msg := msgOrderInProgressNo
			if ordersEnabled {
				msg = msgOrderInProgress
			}
			return errorPage{http.StatusConflict, titleOrderInProgress, msg}
		case codes.FailedPrecondition:
			if strings.HasPrefix(st.Message(), outOfStockPrefix) {
				return errorPage{http.StatusUnprocessableEntity, "Some items are out of stock", readableMessage(st.Message())}
			}
			return errorPage{http.StatusUnprocessableEntity, titleFor(http.StatusUnprocessableEntity), readableMessage(st.Message())}
		case codes.InvalidArgument:
			return errorPage{http.StatusUnprocessableEntity, titleFor(http.StatusUnprocessableEntity), readableMessage(st.Message())}
		case codes.NotFound:
			return errorPage{http.StatusNotFound, titleFor(http.StatusNotFound), msgNotFound}
		case codes.Unavailable, codes.DeadlineExceeded:
			return errorPage{http.StatusServiceUnavailable, titleFor(http.StatusServiceUnavailable), msgUnavailable}
		case codes.Internal:
			if strings.HasPrefix(st.Message(), "failed to charge card") {
				if m := nestedDecline.FindStringSubmatch(st.Message()); m != nil {
					return errorPage{http.StatusUnprocessableEntity, titleFor(http.StatusUnprocessableEntity), readableMessage(m[1])}
				}
			}
		}
		return errorPage{http.StatusInternalServerError, titleFor(http.StatusInternalServerError), msgServerError}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errorPage{http.StatusServiceUnavailable, titleFor(http.StatusServiceUnavailable), msgUnavailable}
	}
	p := errorPage{Code: code, Title: titleFor(code)}
	switch code {
	case http.StatusUnprocessableEntity:
		// our own input validation: the text is written for the visitor
		p.Message = strings.TrimSpace(errors.Cause(err).Error())
	case http.StatusBadRequest:
		p.Message = msgBadRequest
	case http.StatusForbidden:
		p.Message = msgForbidden
	case http.StatusNotFound:
		p.Message = msgNotFound
	case http.StatusServiceUnavailable:
		p.Message = msgUnavailable
	default:
		p = errorPage{http.StatusInternalServerError, titleFor(http.StatusInternalServerError), msgServerError}
	}
	return p
}

// renderHTTPError logs err in full and answers with a page that shows only the
// classified, readable part. htmx requests get the message as a fragment for the
// #page-alert region (htmx would otherwise swap nothing for a 4xx/5xx answer).
func (fe *frontendServer) renderHTTPError(log logrus.FieldLogger, r *http.Request, w http.ResponseWriter, err error, code int) {
	page := classifyError(err, code)
	entry := log.WithField("error", fmt.Sprintf("%+v", err)).WithField("status", page.Code)
	if page.Code >= http.StatusInternalServerError {
		entry.Error("request error")
	} else {
		entry.Warn("request rejected")
	}

	data := map[string]interface{}{
		"title":       page.Title,
		"message":     page.Message,
		"status_code": page.Code,
		"status":      http.StatusText(page.Code),
		"categories":  fe.navCategories(r.Context()),
		"cart_link":   cartLinkFor(page),
	}
	name := "error"
	if r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-History-Restore-Request") != "true" {
		name = "error_alert"
		h := w.Header()
		h.Set("HX-Retarget", "#page-alert")
		h.Set("HX-Reswap", "innerHTML show:#page-alert:top")
		h.Set("HX-Push-Url", "false")
		h.Add("Vary", "HX-Request")
	}
	w.WriteHeader(page.Code)
	if templateErr := getTemplates().ExecuteTemplate(w, name, injectCommonTemplateData(r, data)); templateErr != nil {
		log.Println(templateErr)
	}
}

// cartLinkFor tells whether the error page offers "Back to your cart": the order was refused
// because of the stock (or the checkout attempt is spent), the cart is where to fix it.
func cartLinkFor(p errorPage) bool {
	if p.Code != http.StatusUnprocessableEntity {
		return false
	}
	m := strings.ToLower(p.Message)
	return strings.HasPrefix(m, outOfStockPrefix) || strings.HasPrefix(m, attemptFailedText)
}

// navCache keeps the department list for the header navigation of pages that do
// not need the catalog themselves (cart, order, error pages).
type navCache struct {
	mu   sync.Mutex
	cats []*pb.Category
	till time.Time
}

const (
	navCacheTTL     = 60 * time.Second
	navCacheErrTTL  = 10 * time.Second
	navFetchTimeout = time.Second
)

// navCategories returns the departments for the header nav; nil (no entries) if the
// catalog cannot be asked quickly. Never fails, never slows a page down by more than navFetchTimeout.
func (fe *frontendServer) navCategories(ctx context.Context) []*pb.Category {
	if fe.productCatalogSvcConn == nil {
		return nil
	}
	fe.nav.mu.Lock()
	defer fe.nav.mu.Unlock()
	if time.Now().Before(fe.nav.till) {
		return fe.nav.cats
	}
	ctx, cancel := context.WithTimeout(ctx, navFetchTimeout)
	defer cancel()
	cats, err := fe.listCategories(ctx)
	if err != nil {
		fe.nav.till = time.Now().Add(navCacheErrTTL)
		return fe.nav.cats // last known list, if any
	}
	fe.nav.cats, fe.nav.till = cats, time.Now().Add(navCacheTTL)
	return cats
}
