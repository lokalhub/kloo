// Package tokens estimates how many tokens a string will cost, without shipping
// a tokenizer.
//
// kloo talks to many models — Qwen, DeepSeek, Llama, whatever the local server
// has loaded — and they do not share a tokenizer. Bundling one (tiktoken) would
// add a large dependency to be confidently wrong for most of them. So the
// estimate is a heuristic, corrected against ground truth the endpoint already
// reports back.
//
// Two mechanisms:
//
//   - A base estimate of chars÷ratio, with HIGH-ENTROPY words charged at a much
//     lower ratio. BPE merges are learned from natural text, so a hash or a
//     base64 blob costs roughly one token per two characters while prose costs
//     one per four. Flat chars/4 undercounts a go.sum by 55%, and a budget built
//     on that overflows the server's real context.
//   - A Calibrator that watches reported prompt_tokens and learns the actual
//     chars-per-token ratio for the model in use.
//
// Measured against DeepSeek (chars per real token): Go source 3.75, prose 3.62,
// go.sum hashes 1.78. The constants below are fitted to that, and the fixtures
// in estimate_test.go pin the resulting accuracy.
package tokens

const (
	// DefaultCharsPerToken is the cold-start ratio for ordinary text, before any
	// calibration has happened.
	DefaultCharsPerToken = 4.0
	// denseCharsPerToken is the ratio for high-entropy words (hashes, base64,
	// UUIDs, minified output), measured at ~1.78 on a real go.sum.
	denseCharsPerToken = 1.8

	// A word must be at least this long to be considered high-entropy: short
	// mixed strings ("v1.2.3", "SHA256") tokenize like ordinary text.
	denseMinLen = 16
	// denseTransitionRatio flags a word whose character CLASS changes this often
	// per character — the signature of base64/random data. CamelCase identifiers
	// sit around 0.28–0.39 and import paths around 0.26, so they stay below it.
	denseTransitionRatio = 0.40
	// denseDigitRatio catches the case transitions miss: lowercase hex, which has
	// almost no class changes but is ~50% digits.
	denseDigitRatio = 0.30

	// Calibration is clamped to this band so one odd response cannot wreck the
	// budget arithmetic. 1.5 is denser than any real corpus measured; 5.0 is
	// looser than natural language.
	minCharsPerToken = 1.5
	maxCharsPerToken = 5.0
)

// Estimate returns the token estimate for s at the default ratio.
func Estimate(s string) int { return EstimateAt(s, DefaultCharsPerToken) }

// EstimateAt returns the token estimate for s, charging ordinary words at
// charsPerToken and high-entropy words at denseCharsPerToken. A non-positive
// ratio falls back to the default.
func EstimateAt(s string, charsPerToken float64) int {
	if charsPerToken <= 0 {
		charsPerToken = DefaultCharsPerToken
	}
	if s == "" {
		return 0
	}
	var total float64
	var wordLen, transitions, digits int
	var prev int8 = -1

	flush := func() {
		if wordLen == 0 {
			return
		}
		if isDense(wordLen, transitions, digits) {
			total += float64(wordLen) / denseCharsPerToken
		} else {
			total += float64(wordLen) / charsPerToken
		}
		wordLen, transitions, digits = 0, 0, 0
		prev = -1
	}

	for _, r := range s {
		if r == ' ' || r == '\t' {
			flush()
			total += 1 / charsPerToken // whitespace still costs something
			continue
		}
		if r == '\n' {
			flush()
			total++ // a newline is reliably its own token
			continue
		}
		c := classOf(r)
		if prev >= 0 && c != prev {
			transitions++
		}
		if c == classDigit {
			digits++
		}
		prev = c
		wordLen++
	}
	flush()

	if total < 1 {
		return 1
	}
	return int(total)
}

// isDense reports whether a word's shape says "hash, not prose".
func isDense(length, transitions, digits int) bool {
	if length < denseMinLen {
		return false
	}
	return float64(transitions) >= float64(length)*denseTransitionRatio ||
		float64(digits) >= float64(length)*denseDigitRatio
}

const (
	classLower int8 = iota
	classUpper
	classDigit
	classPunct
)

func classOf(r rune) int8 {
	switch {
	case r >= '0' && r <= '9':
		return classDigit
	case r >= 'a' && r <= 'z':
		return classLower
	case r >= 'A' && r <= 'Z':
		return classUpper
	default:
		return classPunct
	}
}

// Clamp bounds a chars-per-token ratio to the sane band.
func Clamp(ratio float64) float64 {
	if ratio < minCharsPerToken {
		return minCharsPerToken
	}
	if ratio > maxCharsPerToken {
		return maxCharsPerToken
	}
	return ratio
}

// CountChars is the char count the calibrator pairs with a reported token count.
// Exported so callers measure the same thing the estimator does.
func CountChars(parts ...string) int {
	n := 0
	for _, p := range parts {
		n += len([]rune(p))
	}
	return n
}
