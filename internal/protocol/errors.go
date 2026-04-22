package protocol

// JSON-RPC 2.0 reserved codes.
const (
	ErrCodeParseError     = -32700
	ErrCodeInvalidRequest = -32600
	ErrCodeMethodNotFound = -32601
	ErrCodeInvalidParams  = -32602
	ErrCodeInternal       = -32603
)

// Hive-specific error codes live in the -33xxx band to stay clear of
// JSON-RPC's reserved range (-32768 to -32000).
const (
	ErrCodePermissionDenied = -33001
	ErrCodeQuotaExceeded    = -33002
	ErrCodeRoomNotFound     = -33003
	ErrCodePeerNotFound     = -33004
	ErrCodeAgentNotFound    = -33005
	ErrCodeImageNotFound    = -33006
	ErrCodeInvalidManifest  = -33007
	ErrCodeRankViolation    = -33008
)

// Convenience constructors.
func NewError(code int, msg string) *Error { return &Error{Code: code, Message: msg} }

func ErrMethodNotFound(method string) *Error {
	return &Error{Code: ErrCodeMethodNotFound, Message: "method not found: " + method}
}

func ErrInternal(msg string) *Error { return &Error{Code: ErrCodeInternal, Message: msg} }

func ErrPermissionDenied(msg string) *Error {
	return &Error{Code: ErrCodePermissionDenied, Message: msg}
}

func ErrQuotaExceeded(msg string) *Error {
	return &Error{Code: ErrCodeQuotaExceeded, Message: msg}
}
