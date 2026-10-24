package worker

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"strconv"

	"github.com/ogefest/hoarfrost/internal/engine"
	"github.com/ogefest/hoarfrost/internal/recorder"
)

// Reasons of failed captures, a small fixed set usable as a metric label.
const (
	ReasonHTTP4xx   = "http_4xx" // other 4xx statuses
	ReasonHTTP5xx   = "http_5xx"
	ReasonTimeout   = "timeout"
	ReasonNetwork   = "network" // DNS, refused connections, TLS
	ReasonBlocked   = "blocked" // the page's own host is on a block list
	ReasonStorage   = "storage"
	ReasonBlocklist = "blocklist" // block lists could not be loaded
	ReasonEngine    = "engine"    // engine or recorder failure, e.g. Chromium
	ReasonOther     = "other"
)

// reasonOf classifies a capture error. Statuses that need attention on their
// own (401, 403, 404, 410, 429) get their own reason, e.g. "http_401".
func reasonOf(err error) string {
	var se *engine.StatusError
	if errors.As(err, &se) {
		switch s := se.Status; {
		case s == 401 || s == 403 || s == 404 || s == 410 || s == 429:
			return "http_" + strconv.Itoa(s)
		case s >= 500:
			return ReasonHTTP5xx
		default:
			return ReasonHTTP4xx
		}
	}
	var be *recorder.BlockedError
	if errors.As(err, &be) {
		return ReasonBlocked
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ReasonTimeout
	}
	var ne net.Error
	if errors.As(err, &ne) {
		if ne.Timeout() {
			return ReasonTimeout
		}
		return ReasonNetwork
	}
	var ce *tls.CertificateVerificationError
	var he tls.RecordHeaderError
	if errors.As(err, &ce) || errors.As(err, &he) {
		return ReasonNetwork
	}
	return ReasonEngine
}
