package session

import "testing"

func TestRedactSecretsPrefixes(t *testing.T) {
	for in, want := range map[string]string{
		"key sk-abc1 used":                    "key [redacted] used",
		"key sk_abc1 used":                    "key [redacted] used",
		"Authorization: Bearer abc123 sent":   "Authorization: Bearer [redacted] sent",
		"authorization: bearer abc123":        "authorization: bearer [redacted]",
		"BearerXYZ1 seen":                     "[redacted] seen",
		"url?token=abc1 failed":               "[redacted] failed",
		"https://x.test/a?v=1&key=abc failed": "[redacted] failed",
		"with key=v1 and secret=v2":           "with [redacted] and [redacted]",
		"the secret plan and a token= sign":   "the secret plan and a token= sign",
		"ordinary prose stays exactly itself": "ordinary prose stays exactly itself",
		"Bearer":                              "Bearer",
	} {
		if got := redactSecrets(in); got != want {
			t.Errorf("redactSecrets(%q) = %q, want %q", in, got, want)
		}
	}
}
