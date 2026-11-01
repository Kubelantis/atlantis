// Copyright 2017 HootSuite Media Inc.
// SPDX-License-Identifier: Apache-2.0
// Modified hereafter by contributors to runatlantis/atlantis.

package server

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/runatlantis/atlantis/server/logging"
	"github.com/urfave/negroni/v3"
)

// NewRequestLogger creates a RequestLogger.
func NewRequestLogger(s *Server) *RequestLogger {
	return &RequestLogger{
		s.Logger,
		s.WebAuthentication,
		s.WebUsername,
		s.WebPassword,
	}
}

// RequestLogger logs requests and their response codes.
// as well as handle the basicauth on the requests
type RequestLogger struct {
	logger            logging.SimpleLogging
	WebAuthentication bool
	WebUsername       string
	WebPassword       string
}

// maxLoggedUsername bounds the username logged for a failed login, which the
// client chooses.
const maxLoggedUsername = 64

// ServeHTTP implements the middleware function. It logs every request at
// DEBUG level, and at the default level failed logins and requests that can
// change state through the web UI or the API (see changesState), with their
// source address and who made them.
func (l *RequestLogger) ServeHTTP(rw http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
	l.logger.Debug("%s %s – from %s", r.Method, r.URL.RequestURI(), r.RemoteAddr)
	allowed := false
	// actor is who made the request: the web user, "api" (the API token is
	// shared, so it names no one) or "anonymous".
	actor := "anonymous"
	if strings.HasPrefix(r.URL.Path, "/api/") {
		actor = "api"
	}
	if !l.WebAuthentication ||
		r.URL.Path == "/events" ||
		r.URL.Path == "/healthz" ||
		r.URL.Path == "/readyz" ||
		r.URL.Path == "/status" ||
		strings.HasPrefix(r.URL.Path, "/api/") {
		allowed = true
	} else {
		user, pass, ok := r.BasicAuth()
		if ok {
			r.SetBasicAuth(user, pass)
			// Constant-time comparison, so response timing does not reveal
			// how much of the username or password was right.
			userOK := subtle.ConstantTimeCompare([]byte(user), []byte(l.WebUsername)) == 1
			passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(l.WebPassword)) == 1
			if userOK && passOK {
				l.logger.Debug("[VALID] log in: >> url: %s", r.URL.RequestURI())
				allowed = true
				actor = user
			} else {
				allowed = false
				if len(user) > maxLoggedUsername {
					user = user[:maxLoggedUsername] + "..."
				}
				l.logger.Warn("failed web login as %q from %s for %s %s", user, r.RemoteAddr, r.Method, r.URL.RequestURI())
			}
		}
	}
	if !allowed {
		rw.Header().Set("WWW-Authenticate", `Basic realm="restricted", charset="UTF-8"`)
		http.Error(rw, "Unauthorized", http.StatusUnauthorized)
	} else {
		next(rw, r)
	}
	status := rw.(negroni.ResponseWriter).Status()
	if changesState(r) {
		l.logger.Info("%s %s from %s as %q: HTTP %d", r.Method, r.URL.RequestURI(), r.RemoteAddr, actor, status)
	} else {
		l.logger.Debug("%s %s – respond HTTP %d", r.Method, r.URL.RequestURI(), status)
	}
}

// changesState reports whether r goes to the API or can change Atlantis's
// state through the web UI (deleting a lock, the global apply lock, setting
// up a GitHub App). VCS webhooks, page loads and health checks cannot.
func changesState(r *http.Request) bool {
	switch {
	case r.URL.Path == "/events":
		return false
	case strings.HasPrefix(r.URL.Path, "/api/"), strings.HasPrefix(r.URL.Path, "/github-app/"):
		return true
	default:
		return r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions
	}
}
