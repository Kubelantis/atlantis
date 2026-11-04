// Copyright 2017 HootSuite Media Inc.
// SPDX-License-Identifier: Apache-2.0
// Modified hereafter by contributors to runatlantis/atlantis.

package server

import (
	"crypto/subtle"
	"net/http"
	"net/url"
	"slices"
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

// loggedQueryKeys are the query parameters whose values are logged. Others
// can be credentials, such as the GitHub App manifest code on
// /github-app/exchange-code, and are logged as <redacted>.
var loggedQueryKeys = []string{"id"}

// loggedURI is u's path and query for logs, with the values of query
// parameters not in loggedQueryKeys redacted.
func loggedURI(u *url.URL) string {
	uri := u.EscapedPath()
	if u.RawQuery == "" {
		return uri
	}
	var parts []string
	for _, kv := range strings.Split(u.RawQuery, "&") {
		key, _, hasValue := strings.Cut(kv, "=")
		name, err := url.QueryUnescape(key)
		switch {
		case !hasValue:
			parts = append(parts, kv)
		case err == nil && slices.Contains(loggedQueryKeys, name):
			parts = append(parts, kv)
		default:
			parts = append(parts, key+"=<redacted>")
		}
	}
	return uri + "?" + strings.Join(parts, "&")
}

// ServeHTTP implements the middleware function. It logs every request at
// DEBUG level, and at the default level failed logins and requests that can
// change state through the web UI or the API (see changesState), with their
// source address and who made them.
func (l *RequestLogger) ServeHTTP(rw http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
	uri := loggedURI(r.URL)
	l.logger.Debug("%s %s – from %s", r.Method, uri, r.RemoteAddr)
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
				l.logger.Debug("[VALID] log in: >> url: %s", uri)
				allowed = true
				actor = user
			} else {
				allowed = false
				if len(user) > maxLoggedUsername {
					user = user[:maxLoggedUsername] + "..."
				}
				l.logger.Warn("failed web login as %q from %s for %s %s", user, r.RemoteAddr, r.Method, uri)
			}
		} else if r.Header.Get("Authorization") != "" {
			// Credentials were supplied but are not valid Basic auth; their
			// contents are not logged. A request without any is a browser's
			// first try before it prompts, not an attempt.
			l.logger.Warn("failed web login with an unparsable Authorization header from %s for %s %s", r.RemoteAddr, r.Method, uri)
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
		l.logger.Info("%s %s from %s as %q: HTTP %d", r.Method, uri, r.RemoteAddr, actor, status)
	} else {
		l.logger.Debug("%s %s – respond HTTP %d", r.Method, uri, status)
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
