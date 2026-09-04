package store

import (
	"fmt"
	"regexp"
)

var imageIDRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

// ValidImageID rejects path/registry injection in imageId.
func ValidImageID(id string) bool {
	return imageIDRe.MatchString(id)
}

func CheckImageID(id string) error {
	if !ValidImageID(id) {
		return fmt.Errorf("imageId is invalid")
	}
	return nil
}

// secretVersionRe accepts "latest" or a Secret Manager numeric version. It
// rejects anything that could change the resolved Secret Manager REST path
// (internal/secrets/secrets.go interpolates this value unescaped).
var secretVersionRe = regexp.MustCompile(`^[1-9][0-9]*$`)

// ValidSecretVersion reports whether version is "latest", empty (defaults to
// latest), or a positive integer Secret Manager version.
func ValidSecretVersion(version string) bool {
	return version == "" || version == "latest" || secretVersionRe.MatchString(version)
}
