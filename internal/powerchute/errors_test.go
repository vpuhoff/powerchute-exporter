package powerchute

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestSafeRequestDiagnostics(t *testing.T) {
	u, _ := url.Parse("https://example.invalid/j_security_check;jsessionid=private-session?token=private-token")
	raw := errors.New("failed " + u.String() + " password=private-password")
	err := requestFailure(context.Background(), u, raw)
	if ErrorReason(err) != Network || !strings.Contains(ErrorDetail(err), "/j_security_check") {
		t.Fatal("safe endpoint diagnostic missing")
	}
	for _, secret := range []string{"example.invalid", "private-session", "private-token", "private-password", "jsessionid", "?token"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatal("request diagnostic exposed upstream data")
		}
	}
	if ErrorDetail(raw) != "collection failed" {
		t.Fatal("unknown error was logged verbatim")
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if detail := ErrorDetail(requestFailure(ctx, u, context.DeadlineExceeded)); !strings.Contains(detail, "collection cycle deadline exceeded") {
		t.Fatal("cycle timeout was not distinguished")
	}
	if detail := ErrorDetail(requestFailure(context.Background(), u, context.DeadlineExceeded)); !strings.Contains(detail, "request deadline exceeded") {
		t.Fatal("request timeout was not distinguished")
	}
}
