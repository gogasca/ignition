package store

import "strings"

// DomainSubjectPrefix marks a role binding that applies to every Workspace
// user in a domain (matched against the token's `hd` claim). `group:<email>`
// is reserved for a future Cloud Identity group lookup.
const DomainSubjectPrefix = "domain:"

// DomainSubject returns the binding subject for a Workspace domain.
func DomainSubject(domain string) string { return DomainSubjectPrefix + domain }

// RoleBinding is a single project role assignment. Subject is an email, a
// `domain:<d>` selector, or (later) a `group:<g>` selector.
type RoleBinding struct {
	Subject string
	Role    string
}

// TrimDomainSubject reports whether subject is a domain selector and returns
// the bare domain.
func TrimDomainSubject(subject string) (domain string, ok bool) {
	if rest, found := strings.CutPrefix(subject, DomainSubjectPrefix); found {
		return rest, true
	}
	return "", false
}
