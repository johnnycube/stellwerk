package matrix

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"maunium.net/go/mautrix"

	"github.com/johnnycube/stellwerk/internal/core/ports"
)

func TestClassify(t *testing.T) {
	he := func(code string, extra map[string]any) error {
		return mautrix.HTTPError{Response: &http.Response{StatusCode: 400}, RespError: &mautrix.RespError{ErrCode: code, ExtraData: extra}}
	}
	if d, ok := ports.RetryAfterOf(classify(he("M_LIMIT_EXCEEDED", map[string]any{"retry_after_ms": float64(1500)}))); !ok || d != 1500*time.Millisecond {
		t.Errorf("rate limit: %v", d)
	}
	for _, c := range []string{"M_FORBIDDEN", "M_NOT_FOUND", "M_UNKNOWN_TOKEN"} {
		if !ports.IsPermanent(classify(he(c, nil))) {
			t.Errorf("%s must be permanent", c)
		}
	}
	if ports.IsPermanent(classify(he("M_UNKNOWN", nil))) || classify(nil) != nil {
		t.Error("unknown errors are retried")
	}
	plain := errors.New("connection reset")
	if classify(plain) != plain {
		t.Error("plain errors pass through")
	}
}
