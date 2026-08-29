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
