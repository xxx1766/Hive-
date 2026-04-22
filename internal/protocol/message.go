// Package protocol defines the JSON-RPC 2.0 envelope and NDJSON framing
// used by both Hive↔Agent and CLI↔hived channels.
//
// We stick to JSON-RPC 2.0 semantics with one small extension: a server may
// emit unsolicited notifications (no id field) interleaved with responses to
// stream progress — this is used by `room/run` to push log/status events.
package protocol

import "encoding/json"

const Version = "2.0"

// Message is the JSON-RPC 2.0 envelope. A message is either a request
// (Method + ID), a notification (Method only), or a response (ID + Result/Error).
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

func (m *Message) IsRequest() bool      { return m.Method != "" && len(m.ID) > 0 }
func (m *Message) IsNotification() bool { return m.Method != "" && len(m.ID) == 0 }
func (m *Message) IsResponse() bool     { return m.Method == "" && len(m.ID) > 0 }

// Error matches the JSON-RPC 2.0 error object.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string { return e.Message }

// NewRequest builds a request message. id must be a JSON-encodable scalar.
func NewRequest(id any, method string, params any) (*Message, error) {
	idRaw, err := json.Marshal(id)
	if err != nil {
		return nil, err
	}
	var paramsRaw json.RawMessage
	if params != nil {
		paramsRaw, err = json.Marshal(params)
		if err != nil {
			return nil, err
		}
	}
	return &Message{
		JSONRPC: Version,
		ID:      idRaw,
		Method:  method,
		Params:  paramsRaw,
	}, nil
}

// NewNotification builds a notification (request without id).
func NewNotification(method string, params any) (*Message, error) {
	var paramsRaw json.RawMessage
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		paramsRaw = raw
	}
	return &Message{JSONRPC: Version, Method: method, Params: paramsRaw}, nil
}

// NewResponse builds a successful response for the given request id.
func NewResponse(id json.RawMessage, result any) (*Message, error) {
	var resRaw json.RawMessage
	if result != nil {
		r, err := json.Marshal(result)
		if err != nil {
			return nil, err
		}
		resRaw = r
	}
	return &Message{JSONRPC: Version, ID: id, Result: resRaw}, nil
}

// NewErrorResponse builds an error response.
func NewErrorResponse(id json.RawMessage, err *Error) *Message {
	return &Message{JSONRPC: Version, ID: id, Error: err}
}
