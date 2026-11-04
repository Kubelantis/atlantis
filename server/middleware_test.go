// Copyright 2026 The Atlantis Authors
// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/runatlantis/atlantis/server"
	"github.com/runatlantis/atlantis/server/logging"
	. "github.com/runatlantis/atlantis/testing"
	"github.com/urfave/negroni/v3"
)

func TestRequestLogger_WebAuthentication(t *testing.T) {
	requestLogger := server.NewRequestLogger(&server.Server{
		Logger:            logging.NewNoopLogger(t),
		WebAuthentication: true,
		WebUsername:       "user",
		WebPassword:       "password",
	})

	tests := []struct {
		name           string
		path           string
		expStatus      int
		expNextHandler bool
	}{
		{
			name:           "ready endpoint is public",
			path:           "/readyz",
			expStatus:      http.StatusNoContent,
			expNextHandler: true,
		},
		{
			name:           "root remains protected",
			path:           "/",
			expStatus:      http.StatusUnauthorized,
			expNextHandler: false,
		},
		{
			name:           "ready endpoint subpath remains protected",
			path:           "/readyz/details",
			expStatus:      http.StatusUnauthorized,
			expNextHandler: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			responseWriter := negroni.NewResponseWriter(recorder)
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			nextHandlerCalled := false

			requestLogger.ServeHTTP(responseWriter, request, func(w http.ResponseWriter, _ *http.Request) {
				nextHandlerCalled = true
				w.WriteHeader(http.StatusNoContent)
			})

			Equals(t, test.expStatus, recorder.Code)
			Equals(t, test.expNextHandler, nextHandlerCalled)
		})
	}
}

func serveLogged(t *testing.T, method, target string, auth func(*http.Request)) string {
	t.Helper()
	logger := logging.NewNoopLogger(t).WithHistory()
	requestLogger := server.NewRequestLogger(&server.Server{
		Logger:            logger,
		WebAuthentication: true,
		WebUsername:       "user",
		WebPassword:       "password",
	})
	request := httptest.NewRequest(method, target, nil)
	request.RemoteAddr = "203.0.113.5:1234"
	if auth != nil {
		auth(request)
	}
	requestLogger.ServeHTTP(negroni.NewResponseWriter(httptest.NewRecorder()), request, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return logger.GetHistory()
}

func infoOrAbove(history string) []string {
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(history), "\n") {
		if line != "" && !strings.HasPrefix(line, "[DBUG]") {
			lines = append(lines, line)
		}
	}
	return lines
}

// A failed login is visible at the default level, with where it came from
// and the username tried, never the password.
func TestRequestLogger_LogsFailedLogin(t *testing.T) {
	history := serveLogged(t, http.MethodGet, "/", func(r *http.Request) { r.SetBasicAuth("mallory", "guess") })
	Equals(t, []string{`[WARN] failed web login as "mallory" from 203.0.113.5:1234 for GET /`}, infoOrAbove(history))
	Assert(t, !strings.Contains(history, "guess"), "password was logged")
}

// Requests that change state through the UI or the API are logged at the
// default level with who made them and from where.
func TestRequestLogger_LogsStateChangingRequests(t *testing.T) {
	asUser := func(r *http.Request) { r.SetBasicAuth("user", "password") }
	Equals(t, []string{`[INFO] DELETE /locks?id=abc from 203.0.113.5:1234 as "user": HTTP 200`},
		infoOrAbove(serveLogged(t, http.MethodDelete, "/locks?id=abc", asUser)))
	Equals(t, []string{`[INFO] POST /apply/lock from 203.0.113.5:1234 as "user": HTTP 200`},
		infoOrAbove(serveLogged(t, http.MethodPost, "/apply/lock", asUser)))
	Equals(t, []string{`[INFO] POST /api/plan from 203.0.113.5:1234 as "api": HTTP 200`},
		infoOrAbove(serveLogged(t, http.MethodPost, "/api/plan", nil)))
	Equals(t, []string{`[INFO] GET /api/locks from 203.0.113.5:1234 as "api": HTTP 200`},
		infoOrAbove(serveLogged(t, http.MethodGet, "/api/locks", nil)))
	// Refused for lack of credentials: still recorded.
	Equals(t, []string{`[INFO] DELETE /locks?id=abc from 203.0.113.5:1234 as "anonymous": HTTP 401`},
		infoOrAbove(serveLogged(t, http.MethodDelete, "/locks?id=abc", nil)))
}

// Page loads, health checks and VCS webhooks stay at debug.
func TestRequestLogger_RoutineRequestsStayAtDebug(t *testing.T) {
	asUser := func(r *http.Request) { r.SetBasicAuth("user", "password") }
	Equals(t, 0, len(infoOrAbove(serveLogged(t, http.MethodGet, "/", asUser))))
	Equals(t, 0, len(infoOrAbove(serveLogged(t, http.MethodGet, "/healthz", nil))))
	Equals(t, 0, len(infoOrAbove(serveLogged(t, http.MethodPost, "/events", nil))))
}

// Query values can be credentials, such as the GitHub App manifest code on
// /github-app/exchange-code, so only allowlisted ones are logged, at any
// level.
func TestRequestLogger_RedactsQueryValues(t *testing.T) {
	asUser := func(r *http.Request) { r.SetBasicAuth("user", "password") }
	history := serveLogged(t, http.MethodGet, "/github-app/exchange-code?code=s3cr3t-code&state=xyz", asUser)
	Equals(t, []string{`[INFO] GET /github-app/exchange-code?code=<redacted>&state=<redacted> from 203.0.113.5:1234 as "user": HTTP 200`}, infoOrAbove(history))
	Assert(t, !strings.Contains(history, "s3cr3t-code"), "the exchange code was logged: %s", history)
	Assert(t, !strings.Contains(history, "xyz"), "a query value was logged: %s", history)

	// A failed login logs no query value either.
	history = serveLogged(t, http.MethodGet, "/github-app/exchange-code?code=s3cr3t-code", func(r *http.Request) { r.SetBasicAuth("mallory", "guess") })
	Assert(t, !strings.Contains(history, "s3cr3t-code"), "the exchange code was logged: %s", history)
}

// A supplied Authorization header that is not valid Basic credentials is a
// failed login too. It is logged without its contents.
func TestRequestLogger_LogsUnparsableAuthorization(t *testing.T) {
	for _, header := range []string{"Basic !!!not-base64", "Basic bm9jb2xvbg==", "Bearer some-token"} {
		history := serveLogged(t, http.MethodGet, "/", func(r *http.Request) { r.Header.Set("Authorization", header) })
		Equals(t, []string{`[WARN] failed web login with an unparsable Authorization header from 203.0.113.5:1234 for GET /`}, infoOrAbove(history))
		Assert(t, !strings.Contains(history, strings.TrimPrefix(strings.TrimPrefix(header, "Basic "), "Bearer ")), "the header was logged: %s", history)
	}
	// No credentials at all is the browser's first request, not an attempt.
	Equals(t, 0, len(infoOrAbove(serveLogged(t, http.MethodGet, "/", nil))))
}
