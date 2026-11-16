package gateway

// This layer only does lenient "extract from JSON" conversion: upstream field types are
// unstable, so treat anything missing as the zero value and never fail a whole request over one
// malformed field.

// StrOf gets a string; non-strings return the empty string.
func StrOf(v any) string {
	s, _ := v.(string)
	return s
}

// BoolOf gets a bool; non-bools return false.
func BoolOf(v any) bool {
	b, _ := v.(bool)
	return b
}

// ClientError is an error meaning "the request itself is invalid" (mapped to 4xx).
//
// Distinguished from upstream errors: it is not expressed by deepai.HTTPError, and callers use it
// to decide between returning 400 and 502.
type ClientError struct{ Message string }

func (e *ClientError) Error() string { return e.Message }

// ErrNoMessages indicates that no usable messages remain after translation.
var ErrNoMessages = &ClientError{Message: "input 不能为空"}
