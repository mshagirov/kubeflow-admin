package config

import (
	"fmt"
	"net/url"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	// RegistrationKindNFS and RegistrationKindBeeGFS are the only supported
	// registration backends.
	RegistrationKindNFS    = "nfs"
	RegistrationKindBeeGFS = "beegfs"

	// BeeGFSDriverName is the only CSI driver a registration template may use.
	BeeGFSDriverName = "beegfs.csi.netapp.com"

	registrationMaxPathBytes = 4096
	// maxPodDefaultNameBytes keeps the upstream "poddefault-<name>"
	// annotation key's name segment within Kubernetes' 63-character bound.
	maxPodDefaultNameBytes = 52
)

// validateRegistrationTemplate validates one registration template. It is
// deliberately strict: the template is the server-side source of every PV and
// PVC storage value, so an ambiguous or unsafe template must never load.
func validateRegistrationTemplate(t RegistrationTemplate, allowedClasses []string, names map[string]bool) error {
	if t.Name == "" || names[t.Name] || len(validation.IsDNS1123Subdomain(t.Name)) != 0 {
		return fmt.Errorf("invalid or duplicate registration template name")
	}
	if t.Kind != RegistrationKindNFS && t.Kind != RegistrationKindBeeGFS {
		return fmt.Errorf("registration template %q has an unsupported backend", t.Name)
	}
	if t.StorageClass == "" || !stringIn(t.StorageClass, allowedClasses) || len(validation.IsDNS1123Subdomain(t.StorageClass)) != 0 {
		return fmt.Errorf("registration template %q has a StorageClass outside allowedStorageClasses", t.Name)
	}
	pvCapacity, err := positiveQuantity(t.Capacity)
	if err != nil {
		return fmt.Errorf("registration template %q has an invalid PV capacity", t.Name)
	}
	pvcRequest, err := positiveQuantity(t.PVCRequest)
	if err != nil {
		return fmt.Errorf("registration template %q has an invalid PVC request", t.Name)
	}
	if pvcRequest.Cmp(pvCapacity) > 0 {
		return fmt.Errorf("registration template %q PVC request exceeds its PV capacity", t.Name)
	}
	if err := validateAccessModeSet(t.AccessModes, true); err != nil {
		return fmt.Errorf("registration template %q has invalid PV access modes", t.Name)
	}
	if err := validateAccessModeSet(t.PVCAccessModes, true); err != nil {
		return fmt.Errorf("registration template %q has invalid PVC access modes", t.Name)
	}
	for _, mode := range t.PVCAccessModes {
		if !stringIn(mode, t.AccessModes) {
			return fmt.Errorf("registration template %q PVC access mode is not supported by its PV", t.Name)
		}
	}
	if t.ReclaimPolicy != "" && t.ReclaimPolicy != "Retain" {
		return fmt.Errorf("registration template %q only supports Retain reclaim policy", t.Name)
	}
	if t.VolumeMode != "" && t.VolumeMode != "Filesystem" {
		return fmt.Errorf("registration template %q only supports Filesystem volume mode", t.Name)
	}
	if err := validateRegistrationPodDefault(t); err != nil {
		return fmt.Errorf("registration template %q has an invalid PodDefault suggestion", t.Name)
	}
	switch t.Kind {
	case RegistrationKindNFS:
		if t.BeeGFSDriver != "" || t.BeeGFSPrefix != "" {
			return fmt.Errorf("NFS registration template %q must not configure BeeGFS fields", t.Name)
		}
		if t.NFSServer == "" || !validServerText(t.NFSServer) {
			return fmt.Errorf("NFS registration template %q requires a valid NFS server", t.Name)
		}
		if len(t.NFSRoots) == 0 {
			return fmt.Errorf("NFS registration template %q requires at least one approved root", t.Name)
		}
		seen := map[string]bool{}
		for _, root := range t.NFSRoots {
			if !validApprovedRoot(root) || seen[root] {
				return fmt.Errorf("NFS registration template %q has an invalid or duplicate approved root", t.Name)
			}
			seen[root] = true
		}
		if err := validateMountOptions(t.MountOptions); err != nil {
			return fmt.Errorf("NFS registration template %q has invalid mount options", t.Name)
		}
		if !t.RequireDirectoryUIDGID {
			return fmt.Errorf("NFS registration template %q must require directory UID/GID", t.Name)
		}
	case RegistrationKindBeeGFS:
		if t.NFSServer != "" || len(t.NFSRoots) != 0 || len(t.MountOptions) != 0 || t.RequireDirectoryUIDGID {
			return fmt.Errorf("BeeGFS registration template %q must not configure NFS fields", t.Name)
		}
		if t.BeeGFSDriver != BeeGFSDriverName {
			return fmt.Errorf("BeeGFS registration template %q must use the reviewed driver", t.Name)
		}
		if !validBeeGFSPrefix(t.BeeGFSPrefix) {
			return fmt.Errorf("BeeGFS registration template %q has an invalid fixed prefix", t.Name)
		}
	}
	return nil
}

func validateRegistrationPodDefault(t RegistrationTemplate) error {
	if t.PodDefaultName == "" && t.PodDefaultMountPathPrefix == "" {
		return nil
	}
	if t.PodDefaultName == "" || t.PodDefaultMountPathPrefix == "" {
		return fmt.Errorf("name and mount path prefix must be configured together")
	}
	// The final PodDefault name is reused as a selector label key. The
	// upstream admission webhook also derives an annotation whose name segment
	// is "poddefault-<name>" (63 characters), so the reviewed name is capped at
	// 52 bytes independently of that selector.
	if len(t.PodDefaultName) > maxPodDefaultNameBytes || len(validation.IsDNS1123Subdomain(t.PodDefaultName)) != 0 || len(validation.IsQualifiedName(t.PodDefaultName)) != 0 {
		return fmt.Errorf("invalid PodDefault name")
	}
	if !validRegistrationAbsolutePath(t.PodDefaultMountPathPrefix) || t.PodDefaultMountPathPrefix == "/" {
		return fmt.Errorf("invalid PodDefault mount path prefix")
	}
	return nil
}

func positiveQuantity(raw string) (resource.Quantity, error) {
	q, err := resource.ParseQuantity(raw)
	if err != nil || q.Sign() <= 0 || q.Cmp(resource.Quantity{}) <= 0 {
		return resource.Quantity{}, fmt.Errorf("a positive Kubernetes quantity is required")
	}
	return q, nil
}

func validateAccessModeSet(modes []string, required bool) error {
	if required && len(modes) == 0 {
		return fmt.Errorf("at least one access mode is required")
	}
	seen := map[string]bool{}
	for _, mode := range modes {
		switch mode {
		case "ReadWriteOnce", "ReadWriteMany", "ReadOnlyMany", "ReadWriteOncePod":
		default:
			return fmt.Errorf("unsupported access mode")
		}
		if seen[mode] {
			return fmt.Errorf("duplicate access mode")
		}
		seen[mode] = true
	}
	return nil
}

func validateMountOptions(options []string) error {
	seen := map[string]bool{}
	for _, option := range options {
		if option == "" || len(option) > 256 || seen[option] || !validServerText(option) || strings.ContainsAny(option, " \t\r\n") {
			return fmt.Errorf("invalid mount option")
		}
		seen[option] = true
	}
	return nil
}

func validServerText(value string) bool {
	if value == "" || len(value) > 253 || !utf8.ValidString(value) || strings.ContainsAny(value, "/\\") {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

// validApprovedRoot is a canonical, non-root absolute directory. Registration
// templates may approve the root itself; legacy templates still require a
// strict descendant in the adapter.
func validApprovedRoot(root string) bool {
	return validRegistrationAbsolutePath(root) && root != "/"
}

// validRegistrationAbsolutePath rejects ambiguous or non-canonical paths. It
// does not prove anything about server-side symlinks, exports or permissions.
func validRegistrationAbsolutePath(value string) bool {
	if value == "" || len(value) > registrationMaxPathBytes || !utf8.ValidString(value) || strings.Contains(value, "\\") || !path.IsAbs(value) || path.Clean(value) != value {
		return false
	}
	if value == "/" || strings.HasSuffix(value, "/") || strings.Contains(value, "//") {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return !containsEncodedTraversal(value)
}

// validBeeGFSPrefix accepts the fixed scheme, authority and absolute path
// prefix configured by an operator. Credentials, query, fragment and encoded
// traversal are refused.
func validBeeGFSPrefix(value string) bool {
	if value == "" || len(value) > 126 || !utf8.ValidString(value) {
		return false
	}
	if strings.ContainsAny(value, "%\\?#@") {
		return false
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "beegfs" || u.Host == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	if u.Path == "" || u.Path == "/" || path.Clean(u.Path) != u.Path || strings.HasSuffix(u.Path, "/") {
		return false
	}
	if !validRegistrationAbsolutePath(u.Path) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

func containsEncodedTraversal(value string) bool {
	lower := strings.ToLower(value)
	return strings.Contains(lower, "%2e") || strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c")
}

func stringIn(value string, values []string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
