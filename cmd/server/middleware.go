package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"

	"frontend/internal/obs"
)

type ctxKeyLog struct{}
type ctxKeyRequestID struct{}

type logHandler struct {
	log  *logrus.Logger
	next http.Handler
}

type responseRecorder struct {
	b      int
	status int
	w      http.ResponseWriter
}

func (r *responseRecorder) Header() http.Header { return r.w.Header() }

func (r *responseRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.w.Write(p)
	r.b += n
	return n, err
}

func (r *responseRecorder) WriteHeader(statusCode int) {
	r.status = statusCode
	r.w.WriteHeader(statusCode)
}

func (lh *logHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	requestID, _ := uuid.NewRandom()
	ctx = context.WithValue(ctx, ctxKeyRequestID{}, requestID.String())
	// The id goes into the context as well, where the log formatter finds it (request_id) and the gRPC
	// client interceptor forwards it (x-request-id) to the next service.
	ctx = obs.WithRequestID(ctx, requestID.String())

	start := time.Now()
	rr := &responseRecorder{w: w}
	// WithContext(ctx): otelhttp is the outermost handler, so ctx already carries the server span and every
	// line logged through this entry (and the entries derived from it) shows its trace_id and span_id.
	log := lh.log.WithContext(ctx).WithFields(logrus.Fields{
		"http.req.path":   r.URL.Path,
		"http.req.method": r.Method,
	})
	if v, ok := r.Context().Value(ctxKeySessionID{}).(string); ok {
		// The session id is the cookie value (a credential for the visitor's cart and orders): only a
		// short one-way fingerprint is logged, enough to follow one visitor's requests.
		log = log.WithField("session", sessionFingerprint(v))
	}
	log.Debug("request started")
	defer func() {
		// The access line is Info, but not for assets and probes (many per page): those stay Debug.
		level := logrus.InfoLevel
		if isStaticPath(r.URL.Path) {
			level = logrus.DebugLevel
		}
		status := rr.status
		if status == 0 {
			status = http.StatusOK // nothing was written: net/http answers 200
		}
		log.WithFields(logrus.Fields{
			"http.resp.took_ms": int64(time.Since(start) / time.Millisecond),
			"http.resp.status":  status,
			"http.resp.bytes":   rr.b}).Log(level, "request complete")
	}()

	ctx = context.WithValue(ctx, ctxKeyLog{}, log)
	r = r.WithContext(ctx)
	lh.next.ServeHTTP(rr, r)
}

// sessionFingerprint is the first 8 hex characters of the SHA-256 of the session id.
func sessionFingerprint(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:4])
}

func ensureSessionID(next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// assets and probes have no session: a Set-Cookie would make them uncacheable for browsers and CDNs
		if isStaticPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		var sessionID string
		c, err := r.Cookie(cookieSessionID)
		if err == http.ErrNoCookie {
			if os.Getenv("ENABLE_SINGLE_SHARED_SESSION") == "true" {
				// Hard coded user id, shared across sessions
				sessionID = "12345678-1234-1234-1234-123456789123"
			} else {
				u, _ := uuid.NewRandom()
				sessionID = u.String()
			}
			http.SetCookie(w, &http.Cookie{
				Name:     cookieSessionID,
				Value:    sessionID,
				MaxAge:   cookieMaxAge,
				HttpOnly: true,
				Secure:   true,
				SameSite: http.SameSiteStrictMode,
			})
		} else if err != nil {
			return
		} else {
			sessionID = c.Value
		}
		ctx := context.WithValue(r.Context(), ctxKeySessionID{}, sessionID)
		r = r.WithContext(ctx)
		next.ServeHTTP(w, r)
	}
}
