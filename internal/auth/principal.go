package auth

import "strings"

// SubjectKind distinguishes a human user from a workload identity.
type SubjectKind string

const (
	// KindUser is a human identity (Workspace or consumer Google account).
	KindUser SubjectKind = "user"
	// KindServiceAccount is a GCP service account (Workload Identity, key,
	// or impersonation).
	KindServiceAccount SubjectKind = "service_account"
)

// gcpServiceAccountSuffix ends every GCP service-account email
// (`name@project.iam.gserviceaccount.com`, and the Google-managed
// `...@*.gserviceaccount.com` variants).
const gcpServiceAccountSuffix = ".gserviceaccount.com"

// Principal is the authenticated caller.
type Principal struct {
	// Subject is the stable identifier used for RBAC lookups and audit
	// records. For Google identities it is the verified email address; for
	// first-party RFC 9068 access tokens it is the `sub` claim.
	Subject string
	// Email is the caller's email, when the token carried one. Lower-cased.
	Email string
	// Kind is user or service_account, derived from Email. Empty when the
	// token carried no email (first-party sub-only tokens).
	Kind SubjectKind
	// Domain is the Google Workspace primary domain (the `hd` claim). Set
	// for Workspace users only; empty for consumer accounts and service
	// accounts.
	Domain string
	// Client is the authorized party (`azp` / `client_id` claim), when present.
	Client string
}

// IsServiceAccount reports whether the principal is a GCP service account.
func (p Principal) IsServiceAccount() bool { return p.Kind == KindServiceAccount }

// ClassifySubject maps an email to a SubjectKind. An empty email returns the
// empty kind so callers can tell "no email" apart from "user".
func ClassifySubject(email string) SubjectKind {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return ""
	}
	if strings.HasSuffix(email, gcpServiceAccountSuffix) {
		return KindServiceAccount
	}
	return KindUser
}
