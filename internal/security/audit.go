// Package security provides secret detection and sanitization.
package security

import (
	"regexp"
	"strings"
)

// SecretsSanitizer detects and redacts common secret patterns.
type SecretsSanitizer struct {
	patterns []*regexp.Regexp
}

func NewSecretsSanitizer() *SecretsSanitizer {
	return &SecretsSanitizer{
		patterns: []*regexp.Regexp{
			regexp.MustCompile(`(?i)(api[_-]?key|apikey|secret|token|password|passwd|auth)\s*[:=]\s*['"]?\S+['"]?`),
			regexp.MustCompile(`(?i)sk-[a-zA-Z0-9]{20,}`),
			regexp.MustCompile(`(?i)eyJ[a-zA-Z0-9_-]{20,}\.[a-zA-Z0-9_-]{20,}\.[a-zA-Z0-9_-]{20,}`),
			regexp.MustCompile(`ghp_[a-zA-Z0-9]{36}`),
			regexp.MustCompile(`glpat-[a-zA-Z0-9_-]{20,}`),
			regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
		},
	}
}

func (s *SecretsSanitizer) Detect(content string) bool {
	for _, pat := range s.patterns {
		if pat.MatchString(content) {
			return true
		}
	}
	return false
}

func (s *SecretsSanitizer) Sanitize(content string) string {
	result := content
	for _, pat := range s.patterns {
		result = pat.ReplaceAllStringFunc(result, func(match string) string {
			if len(match) > 20 {
				return match[:8] + strings.Repeat("*", 12)
			}
			return "***REDACTED***"
		})
	}
	return result
}

// RedactTokenValues removes known sensitive env var values from output.
type Redactor struct {
	tokens []string
}

func NewRedactor(tokens ...string) *Redactor {
	return &Redactor{tokens: tokens}
}

func (r *Redactor) Redact(s string) string {
	for _, tok := range r.tokens {
		if tok == "" {
			continue
		}
		s = strings.ReplaceAll(s, tok, "***")
	}
	return s
}
