package llm

import (
	"slices"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/config"
)

// The real page that ended a run: a CDN 525 interstitial, several KB of markup,
// inline script and an IP-reveal widget. Quoted whole it went into the transcript
// and the model's context.
const cdnErrorPage = `<!DOCTYPE html><html lang="en-US"><head>
<title>lokalai.example.com | 525: SSL handshake failed</title>
<meta charset="UTF-8" /></head><body>
<div class="error-footer"><p><span class="cf-footer-item">Cloudflare Ray ID: <strong>abc123</strong></span>
<button type="button" id="cf-footer-ip-reveal">Click to reveal</button>
<span class="hidden" id="cf-footer-ip">203.0.113.7</span></p>
<script>(function(){var a=document;a.addEventListener&&a.addEventListener("DOMContentLoaded",function(){})})();</script>
</div></body></html>`

func TestAPIErrorCollapsesHTMLPageToItsTitle(t *testing.T) {
	err := (&APIError{StatusCode: 525, Status: "525 ", Body: cdnErrorPage}).Error()
	if strings.Contains(err, "<script") || strings.Contains(err, "cf-footer") {
		t.Fatalf("the HTML page was quoted into the error: %s", err)
	}
	if !strings.Contains(err, "525: SSL handshake failed") {
		t.Errorf("the page title — the one useful line — was dropped: %s", err)
	}
	if len(err) > 200 {
		t.Errorf("error is %d chars, want a one-liner: %s", len(err), err)
	}
}

// A JSON body is where small-model endpoints put the useful detail, so it must
// still come through verbatim.
func TestAPIErrorKeepsShortJSONBodyVerbatim(t *testing.T) {
	body := `{"error":{"message":"upstream is at capacity","code":"upstream_at_capacity"}}`
	err := (&APIError{StatusCode: 503, Body: body}).Error()
	if !strings.Contains(err, body) {
		t.Errorf("JSON body was altered: %s", err)
	}
}

func TestAPIErrorTruncatesAnOversizeNonHTMLBody(t *testing.T) {
	err := (&APIError{StatusCode: 500, Body: strings.Repeat("x", 5000)}).Error()
	if len(err) > errorBodyMax+100 {
		t.Errorf("oversize body was not truncated: %d chars", len(err))
	}
	if !strings.Contains(err, "5000 bytes") {
		t.Errorf("truncation does not say how much was dropped: %s", err)
	}
}

// 525 and 526 are as transient as the 520-524 already on the ladder. A real run
// died mid-task on a 525 from an endpoint that answered 200 on the next attempt.
func TestSSLHandshakeStatusesAreRetryable(t *testing.T) {
	for _, code := range []int{520, 521, 522, 523, 524, 525, 526, 527, 530} {
		if !slices.Contains(config.DefaultLLMRetryableStatusCodes, code) {
			t.Errorf("status %d is not retryable", code)
		}
	}
	for _, code := range []int{400, 401, 403, 404, 422} {
		if slices.Contains(config.DefaultLLMRetryableStatusCodes, code) {
			t.Errorf("status %d must NOT be retried — retrying a client error just burns the budget", code)
		}
	}
}
