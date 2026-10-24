package worker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"testing"

	"github.com/ogefest/hoarfrost/internal/engine"
	"github.com/ogefest/hoarfrost/internal/recorder"
)

func TestReasonOf(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{&engine.StatusError{URL: "u", Status: 401}, "http_401"},
		{fmt.Errorf("wrapped: %w", &engine.StatusError{URL: "u", Status: 404}), "http_404"},
		{&engine.StatusError{URL: "u", Status: 418}, ReasonHTTP4xx},
		{&engine.StatusError{URL: "u", Status: 503}, ReasonHTTP5xx},
		{&recorder.BlockedError{Host: "ads.example"}, ReasonBlocked},
		{context.DeadlineExceeded, ReasonTimeout},
		{&url.Error{Op: "Get", URL: "u", Err: os.ErrDeadlineExceeded}, ReasonTimeout},
		{&url.Error{Op: "Get", URL: "u", Err: &net.OpError{Op: "dial", Err: errors.New("connection refused")}}, ReasonNetwork},
		{&net.DNSError{Err: "no such host", Name: "x.example"}, ReasonNetwork},
		{errors.New("chrome: launch: no browser"), ReasonEngine},
	} {
		if got := reasonOf(c.err); got != c.want {
			t.Errorf("reasonOf(%v) = %s, want %s", c.err, got, c.want)
		}
	}
}
