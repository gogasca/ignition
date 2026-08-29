package api

import (
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const streamTokenType = "stream+jwt"

func signStreamToken(secret, audience, subject, projectID, sandboxID, processID string, generation, epoch int64, now, exp time.Time) (string, error) {
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iss":          "ignition-api",
		"aud":          audience,
		"sub":          subject,
		"exp":          exp.Unix(),
		"nbf":          now.Unix(),
		"iat":          now.Unix(),
		"project_id":   projectID,
		"sandbox_id":   sandboxID,
		"generation":   generation,
		"process_id":   processID,
		"stream_epoch": epoch,
		"action":       "attach",
	})
	tok.Header["typ"] = streamTokenType
	return tok.SignedString([]byte(secret))
}
