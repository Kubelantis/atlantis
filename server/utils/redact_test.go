package utils_test

import (
	"testing"

	"github.com/runatlantis/atlantis/server/utils"
	. "github.com/runatlantis/atlantis/testing"
)

func TestRedactURLCredentials(t *testing.T) {
	cases := map[string]string{
		"Error: git::https://user:s3cret@github.com/org/mod.git":  "Error: git::https://<redacted>@github.com/org/mod.git",
		"clone https://x-access-token:ghs_abc@github.com/o/r.git": "clone https://<redacted>@github.com/o/r.git",
		"fetch https://ghp_token123@github.com/o/r failed":        "fetch https://<redacted>@github.com/o/r failed",
		"HTTP://USER:PW@example.com":                              "HTTP://<redacted>@example.com",
		"two: https://a:b@h1/x and http://c@h2/y":                 "two: https://<redacted>@h1/x and http://<redacted>@h2/y",
		"no credentials: https://github.com/org/repo":             "no credentials: https://github.com/org/repo",
		"an email in text: ops@example.com":                       "an email in text: ops@example.com",
		"ssh: git@github.com:org/repo.git":                        "ssh: git@github.com:org/repo.git",
		"":                                                        "",
	}
	for in, want := range cases {
		Equals(t, want, utils.RedactURLCredentials(in))
	}
}
