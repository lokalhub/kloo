package llm

import (
	"bytes"
	"fmt"
	"io"
	"strings"
)

const maxAPIErrorBodyBytes = 8192

func readAPIErrorBody(r io.Reader, secrets ...string) string {
	body, truncated := readTail(r, maxAPIErrorBodyBytes)
	text := redactSecrets(string(body), secrets...)
	if !truncated {
		return text
	}
	return fmt.Sprintf("[upstream body truncated; showing last %d bytes]\n%s", maxAPIErrorBodyBytes, text)
}

func redactSecrets(s string, secrets ...string) string {
	for _, secret := range secrets {
		secret = strings.TrimSpace(secret)
		if secret == "" {
			continue
		}
		s = strings.ReplaceAll(s, "Bearer "+secret, "Bearer [REDACTED]")
		s = strings.ReplaceAll(s, secret, "[REDACTED]")
	}
	return s
}

func readTail(r io.Reader, maxBytes int) ([]byte, bool) {
	if maxBytes <= 0 {
		_, _ = io.Copy(io.Discard, r)
		return nil, true
	}
	var buf bytes.Buffer
	tmp := make([]byte, 4096)
	truncated := false
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
			if buf.Len() > maxBytes {
				truncated = true
				b := buf.Bytes()
				buf.Reset()
				buf.Write(b[len(b)-maxBytes:])
			}
		}
		if err == io.EOF {
			return append([]byte(nil), buf.Bytes()...), truncated
		}
		if err != nil {
			return append([]byte(nil), buf.Bytes()...), truncated
		}
	}
}
