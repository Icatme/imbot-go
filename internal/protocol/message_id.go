package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

// MessageID preserves opaque string identifiers and unsigned 64-bit JSON
// integers without passing through a floating-point or signed representation.
// It marshals as a JSON string. Empty and zero IDs mean no server identity.
type MessageID string

func (id *MessageID) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("null")) {
		*id = ""
		return nil
	}
	var value string
	if len(data) > 0 && data[0] == '"' {
		if err := json.Unmarshal(data, &value); err != nil {
			return err
		}
	} else {
		n, err := strconv.ParseUint(string(data), 10, 64)
		if err != nil {
			return fmt.Errorf("message ID must be a string or uint64")
		}
		value = strconv.FormatUint(n, 10)
	}
	if value == "0" {
		value = ""
	}
	*id = MessageID(value)
	return nil
}
