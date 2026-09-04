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
	PermImageCreate      Permission = "image.create"
	PermImageGet         Permission = "image.get"
	PermRoleBindingGet   Permission = "rolebinding.get"
	PermRoleBindingAdmin Permission = "rolebinding.admin"
)

const (
	RoleOwner     = "owner"
	RoleAdmin     = "admin"
	RoleDeveloper = "developer"
	RoleOperator  = "operator"
	RoleViewer    = "viewer"
)

// KnownRole reports whether role is one of the five defined project roles.
func KnownRole(role string) bool {
	switch role {
	case RoleOwner, RoleAdmin, RoleDeveloper, RoleOperator, RoleViewer:
		return true
	default:
		return false
	}
}

// Allowed reports whether role may perform perm.
// own is true when the principal created the target resource (developer terminate/cancel).
func Allowed(role string, perm Permission, own bool) bool {
	switch perm {
	case PermRoleBindingGet, PermRoleBindingAdmin:
		// Project role management is limited to owners and admins.
		return role == RoleOwner || role == RoleAdmin
	case PermSandboxGet, PermProcessGet, PermOperationGet, PermRuntimeGet, PermImageGet:
		return role == RoleOwner || role == RoleAdmin || role == RoleDeveloper || role == RoleOperator || role == RoleViewer
	case PermSandboxCreate, PermSandboxExec, PermImageCreate:
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
