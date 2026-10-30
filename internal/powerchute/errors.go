package powerchute

import "errors"

type Reason string

const (
	Network        Reason = "network"
	TLS            Reason = "tls"
	Authentication Reason = "authentication"
	HTTP           Reason = "http"
	Parse          Reason = "parse"
	Empty          Reason = "empty"
	Period         Reason = "period"
	Timestamp      Reason = "timestamp"
)

var Reasons = []Reason{Network, TLS, Authentication, HTTP, Parse, Empty, Period, Timestamp}

// Error never retains response bodies, URLs, credentials or underlying HTTP errors.
type Error struct {
	Reason  Reason
	Message string
}

func (e *Error) Error() string        { return string(e.Reason) + ": " + e.Message }
func fail(r Reason, msg string) error { return &Error{r, msg} }
func ErrorReason(err error) Reason {
	var e *Error
	if errors.As(err, &e) {
		return e.Reason
	}
	return Parse
}
