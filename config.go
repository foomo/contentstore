package contentstore

import (
	"fmt"
	"slices"
)

// DeploymentRole is the immutable safety role of one Content Store instance.
type DeploymentRole string

const (
	DeploymentRoleCanonical DeploymentRole = "canonical"
	DeploymentRoleSandbox   DeploymentRole = "sandbox"
)

// ParseDeploymentRole validates a configured deployment role.
func ParseDeploymentRole(value string) (DeploymentRole, error) {
	role := DeploymentRole(value)
	if role != DeploymentRoleCanonical && role != DeploymentRoleSandbox {
		return "", fmt.Errorf("contentstore: deployment role must be %q or %q", DeploymentRoleCanonical, DeploymentRoleSandbox)
	}
	return role, nil
}

// LocaleConfig declares the locales the engine validates against. It is injected
// so the core does not depend on any project's locale package.
type LocaleConfig struct {
	Default   string
	Supported []string
}

func (c LocaleConfig) isSupported(locale string) bool {
	return slices.Contains(c.Supported, locale)
}

// Content-change operations reported to a Notifier.
const (
	OpSaveDraft    = "saveDraft"
	OpPublish      = "publish"
	OpUnpublish    = "unpublish"
	OpArchive      = "archive"
	OpUnarchive    = "unarchive"
	OpDelete       = "delete"
	OpContentReset = "contentReset"
)

// ContentChange describes a successful content write so consuming applications
// can invalidate the configured published or preview representation.
type ContentChange struct {
	Type     ContentType
	OwnerRef *OwnerRef
	Op       string
}

// Notifier is a best-effort hook invoked on content changes. It must not
// fail the originating write; implementations handle their own errors. A nil
// Notifier disables emission.
type Notifier func(ContentChange)

// Deps are the injected collaborators an Engine needs. Everything project- or
// platform-specific (persistence, schemas, locales, ID generation, change
// notification) is supplied here so the core stays dependency-free.
type Deps struct {
	Store   Store
	Schemas *Registry
	Locales LocaleConfig
	NewID   func() string
	Notify  Notifier
	Role    DeploymentRole
}
