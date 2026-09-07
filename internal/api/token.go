package api

import (
	"time"

	"ignition.dev/ignition/internal/streamtoken"
)

func signStreamToken(secret, audience, subject, projectID, sandboxID, processID string, generation, epoch int64, now, exp time.Time) (string, error) {
	return streamtoken.Sign(secret, streamtoken.Claims{
		Subject:     subject,
		Audience:    audience,
		ProjectID:   projectID,
		SandboxID:   sandboxID,
		ProcessID:   processID,
		Generation:  generation,
		StreamEpoch: epoch,
		Action:      streamtoken.ActionAttach,
		IssuedAt:    now,
		NotBefore:   now,
		ExpiresAt:   exp,
	})
}
