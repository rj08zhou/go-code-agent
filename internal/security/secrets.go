package security

import "regexp"

// P0-4: Secrets Sanitizer - detect and redact secrets in text

// SecretsSanitizer detects and redacts sensitive information (API keys,
// tokens, passwords, private keys) from text.
type SecretsSanitizer struct {
	patterns []*regexp.Regexp
}

// NewSecretsSanitizer creates a new sanitizer with built-in patterns.
func NewSecretsSanitizer() *SecretsSanitizer {
	patterns := []*regexp.Regexp{
		// OpenAI-style API keys (sk-...)
		regexp.MustCompile(`(?i)(sk-(?:proj-|)?[a-zA-Z0-9_-]{10,})`),

		// Generic api_key / apiKey patterns with long values
		regexp.MustCompile(`(?i)(api[_\-]?key|apikey)["\s]*[:=]["\s]*["']?[a-zA-Z0-9_\-./+=]{20,}["']?`),

		// AWS Access Key ID
		regexp.MustCompile(`(A3T[A-Z0-9]|AKIA|AGPA|AIDA|AROA|AIPA|ANPA|ANVA|ASIA)[A-Z0-9]{16}`),

		// AWS Secret Access Key (40 char base64-ish)
		regexp.MustCompile(`(?i)(aws[_\-]?secret[_\-]?access[_\-]?key)["\s]*[:=]["\s]*["']?[A-Za-z0-9/+=]{40}["']?`),

		// Generic tokens / bearer tokens
		regexp.MustCompile(`(?i)(token|auth[_\-]?token|bearer|access[_\-]?token)["\s]*[:=]\s*["']?[\w\-._~+/]{20,}["']?`),

		// Passwords in URLs (user:pass@host)
		regexp.MustCompile(`(https?://)[^/\s:@]+:[^/\s@]+@`),

		// Private keys (PEM format)
		regexp.MustCompile(`(?s)-----BEGIN\s*(RSA |EC |DSA |OPENSSH |PRIVATE )?KEY-----[\s\S]*?-----END\s*(RSA |EC |DSA |OPENSSH |PRIVATE )?KEY-----`),
	}
	return &SecretsSanitizer{patterns: patterns}
}

// Detect returns true if the text potentially contains secrets.
func (s *SecretsSanitizer) Detect(text string) bool {
	for _, pat := range s.patterns {
		if pat.MatchString(text) {
			return true
		}
	}
	return false
}

// DetectPatterns returns descriptions of matched secret patterns (for warning).
func (s *SecretsSanitizer) DetectPatterns(text string) []string {
	var found []string
	for _, pat := range s.patterns {
		if pat.MatchString(text) {
			found = append(found, pat.String())
		}
	}
	return found
}

// Sanitize redacts detected secrets from text, preserving prefix for identification.
func (s *SecretsSanitizer) Sanitize(text string) string {
	result := text
	for _, pat := range s.patterns {
		result = pat.ReplaceAllStringFunc(result, func(match string) string {
			if len(match) > 12 {
				// Keep first few chars so logs are debuggable
				return match[:6] + "***REDACTED***"
			}
			return "***REDACTED***"
		})
	}
	return result
}

// PatternCount reports how many secret-detection patterns are loaded.
// Exposed so callers (e.g. the /security REPL command) can report on
// the sanitizer without reaching into the unexported patterns field.
func (s *SecretsSanitizer) PatternCount() int {
	return len(s.patterns)
}

// GlobalSecretsSanitizer is the process-wide sanitizer instance.
var GlobalSecretsSanitizer = NewSecretsSanitizer()
