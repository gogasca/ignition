package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

func readBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	b, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, fmt.Errorf("request body exceeds %d bytes", limit)
		}
		return nil, fmt.Errorf("could not read request body")
	}
	return b, nil
}

func pageSize(w http.ResponseWriter, r *http.Request, requestID string) (int, bool) {
	raw := r.URL.Query().Get("pageSize")
	if raw == "" {
		return 0, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > 1000 {
		writeStatus(w, requestID, http.StatusBadRequest, "INVALID_ARGUMENT", "pageSize must be between 1 and 1000", false, 0)
		return 0, false
	}
	return n, true
}

// decodeJSON unmarshals exactly one JSON value. Unknown fields are ignored so
// that a newer client talking to an older server stays forward-compatible.
func decodeJSON(raw []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	if err := d.Decode(dst); err != nil {
		return err
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("request body must contain one JSON value")
	}
	return nil
}
