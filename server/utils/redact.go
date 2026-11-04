// Copyright 2025 The Atlantis Authors
// SPDX-License-Identifier: Apache-2.0

package utils

import (
	"regexp"
	"strings"
)

// urlCredentialRE matches the userinfo of http(s) URLs. A username alone is
// matched too: tokens are often passed as the username
// (https://<token>@github.com/...).
var urlCredentialRE = regexp.MustCompile(`(?i)(https?://)([^\s/@]+:)?[^\s/@]+@`)

// RedactURLCredentials replaces the credentials of every http(s) URL in s
// with <redacted>. Use it on text that reaches logs or pull request comments,
// such as command output and errors.
func RedactURLCredentials(s string) string {
	if !strings.Contains(s, "@") {
		return s
	}
	return urlCredentialRE.ReplaceAllString(s, "$1<redacted>@")
}
