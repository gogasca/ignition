package auth

// Permission is a project-scoped action.
type Permission string

const (
	PermSandboxCreate    Permission = "sandbox.create"
	PermSandboxGet       Permission = "sandbox.get"
	PermSandboxTerminate Permission = "sandbox.terminate"
	PermSandboxExec      Permission = "sandbox.exec"
	PermProcessGet       Permission = "process.get"
	PermOperationGet     Permission = "operation.get"
	PermOperationCancel  Permission = "operation.cancel"
	PermRuntimeGet       Permission = "runtime.get"
)

const (
	RoleOwner     = "owner"
	RoleAdmin     = "admin"
	RoleDeveloper = "developer"
	RoleOperator  = "operator"
	RoleViewer    = "viewer"
)

// Principal is the authenticated caller.
type Principal struct {
	Subject string
	Client  string
}

// Allowed reports whether role may perform perm.
// own is true when the principal created the target resource (developer terminate/cancel).
func Allowed(role string, perm Permission, own bool) bool {
	switch perm {
	case PermSandboxGet, PermProcessGet, PermOperationGet, PermRuntimeGet:
		return role == RoleOwner || role == RoleAdmin || role == RoleDeveloper || role == RoleOperator || role == RoleViewer
	case PermSandboxCreate, PermSandboxExec:
		return role == RoleOwner || role == RoleAdmin || role == RoleDeveloper || role == RoleOperator
	case PermSandboxTerminate, PermOperationCancel:
		if role == RoleOwner || role == RoleAdmin || role == RoleOperator {
			return true
		}
		return role == RoleDeveloper && own
	default:
		return false
	}
}
