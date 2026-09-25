package domain

import (
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Role of a panel user. v1 has a single administrator role (D64).
type Role string

// RoleAdmin can do everything on the node.
const RoleAdmin Role = "admin"

// User is a panel user.
type User struct {
	ID        string
	Username  string
	Role      Role
	Disabled  bool
	CreatedAt time.Time
}

// MinPasswordLength applies to panel users.
const MinPasswordLength = 10

// ValidateUsername checks a panel or camera username.
func ValidateUsername(field, name string) error {
	if name == "" {
		return Invalid(field, "is required")
	}
	if utf8.RuneCountInString(name) > 32 {
		return Invalid(field, "must be at most 32 characters")
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-@", r)) {
			return Invalid(field, "may only contain letters, digits and . _ - @")
		}
	}
	return nil
}

// ValidatePanelPassword enforces the panel password policy.
func ValidatePanelPassword(pw string) error {
	if utf8.RuneCountInString(pw) < MinPasswordLength {
		return Invalid("password", "must be at least %d characters", MinPasswordLength)
	}
	if len(pw) > 256 {
		return Invalid("password", "must be at most 256 bytes")
	}
	return nil
}

// CameraUser is a user of a simulated camera (RN-11, D11). Cameras accept
// weak factory passwords on purpose: they imitate real devices.
type CameraUser struct {
	ID       string
	Username string
	Password string
	Role     string
}

// ValidateCameraUsers checks RN-11: at least one administrator, unique names.
func ValidateCameraUsers(users []CameraUser) error {
	v := &ValidationError{}
	seen := map[string]bool{}
	admins := 0
	for i, u := range users {
		field := "users[" + strconv.Itoa(i) + "]"
		if err := ValidateUsername(field+".username", u.Username); err != nil {
			v.Fields = append(v.Fields, err.(*ValidationError).Fields...)
		}
		if u.Password == "" || len(u.Password) > 64 {
			v.Add(field+".password", "is required and must be at most 64 bytes")
		}
		if strings.ContainsAny(u.Password, "\r\n\x00") {
			v.Add(field+".password", "must not contain control characters")
		}
		switch u.Role {
		case "admin":
			admins++
		case "operator", "viewer":
		default:
			v.Add(field+".role", "must be admin, operator or viewer")
		}
		key := strings.ToLower(u.Username)
		if seen[key] {
			v.Add(field+".username", "is duplicated")
		}
		seen[key] = true
	}
	if admins == 0 {
		v.Add("users", "at least one admin user is required")
	}
	return v.Err()
}
