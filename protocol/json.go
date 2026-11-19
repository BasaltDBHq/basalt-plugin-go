package protocol

import (
	"bytes"
	"encoding/json"
)

// Marshal encodes v as compact JSON, as json.Marshal does but without
// escaping <, > and &. Transports encode messages with it, so the bytes of a
// notifier event reach a plugin as the engine built them, on every transport.
func Marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte{'\n'}), nil
}
