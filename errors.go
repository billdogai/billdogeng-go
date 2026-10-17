package billdogeng

import "fmt"

// Error is returned when an API request ultimately fails (after retries).
type Error struct {
	// Message is a human-readable description of the failure.
	Message string
	// Status is the HTTP status code, or 0 for network/timeout failures.
	Status int
	// Code is the machine-readable error code from the API envelope, if any.
	Code string
	// Body is the raw response body (parsed if JSON, else string), if any.
	Body interface{}
}

func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("billdogeng: %s (status %d, code %s)", e.Message, e.Status, e.Code)
	}
	return fmt.Sprintf("billdogeng: %s (status %d)", e.Message, e.Status)
}

// Is reports whether this error matches a package sentinel, so callers can write
// errors.Is(err, ErrQuotaLimited) rather than compare Code strings.
func (e *Error) Is(target error) bool {
	return target == ErrQuotaLimited && e.Code == quotaLimitedCode
}

func newError(message string, status int, code string, body interface{}) *Error {
	return &Error{Message: message, Status: status, Code: code, Body: body}
}
