package reconcile

import (
	"encoding/json"
	"io"
)

func decodeJSON(r io.Reader, v any) error {
	return json.NewDecoder(io.LimitReader(r, 1<<20)).Decode(v)
}
