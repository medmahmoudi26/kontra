package checkpoint

import (
	"encoding/json"
	"errors"
	"strconv"
)

// Kept in one file so checkpoint.go reads as the contract and nothing else.
var errBadRange = errors.New("checkpoint: a range must be exactly [lo, hi]")

func itoa(i int) string { return strconv.Itoa(i) }

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
