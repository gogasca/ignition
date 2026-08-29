package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

func canonicalHash(method, route string, body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	var v any
	if trimmed == "" {
		v = map[string]any{}
	} else {
		_ = json.Unmarshal(body, &v)
	}
	canon, _ := json.Marshal(v)
	sum := sha256.Sum256([]byte(method + "\n" + route + "\n" + string(canon)))
	return hex.EncodeToString(sum[:])
}
