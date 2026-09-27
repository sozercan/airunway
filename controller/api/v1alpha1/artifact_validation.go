package v1alpha1

import (
	"fmt"
	"net/url"
	"path"
	"reflect"
	"regexp"
	"strings"
	"unicode"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// ArtifactDirectory is the staging directory relative to the modelCache mount.
const ArtifactDirectory = "artifacts"

var artifactRepository = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$`)
var artifactTag = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}$`)
var artifactDigest = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
var artifactHFPart = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.-]*$`)

// ArtifactCacheVolume returns the cache volume, if configured. ValidateArtifact
// rejects multiple caches and unusable volumes before a download can start.
func (s *ModelDeploymentSpec) ArtifactCacheVolume() *StorageVolume {
	if s.Model.Storage != nil {
		for i := range s.Model.Storage.Volumes {
			if s.Model.Storage.Volumes[i].Purpose == VolumePurposeModelCache {
				return &s.Model.Storage.Volumes[i]
			}
		}
	}
	return nil
}

// ArtifactPath is shared by admission, download Jobs, and provider transforms.
func (s *ModelDeploymentSpec) ArtifactPath() string {
	mount := "/model-cache"
	if vol := s.ArtifactCacheVolume(); vol != nil && vol.MountPath != "" {
		mount = vol.MountPath
	}
	return path.Join(mount, ArtifactDirectory)
}

func safeArtifactPath(value string) bool {
	if value == "" || len(value) > 1024 || strings.ContainsAny(value, `\%`) || strings.HasPrefix(value, "/") {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	for part := range strings.SplitSeq(value, "/") {
		if part == "" || part == "." || part == ".." || part == "._airunway_complete.json" {
			return false
		}
	}
	return true
}

// validArtifactReference accepts distribution repository names with optional
// registry/port and an explicit tag or sha256 digest. It does not accept URLs.
func validArtifactReference(value string) bool {
	if value == "" || len(value) > 512 || strings.ContainsAny(value, "\\%?# \t\r\n") {
		return false
	}
	name := value
	if i := strings.IndexByte(name, '@'); i >= 0 {
		if !artifactDigest.MatchString(name[i+1:]) {
			return false
		}
		name = name[:i]
	} else {
		i := strings.LastIndexByte(name, ':')
		if i <= strings.LastIndexByte(name, '/') || !artifactTag.MatchString(name[i+1:]) {
			return false
		}
		name = name[:i]
	}
	// An optional tag may precede a digest.
	if i := strings.LastIndexByte(name, ':'); i > strings.LastIndexByte(name, '/') {
		if !artifactTag.MatchString(name[i+1:]) {
			return false
		}
		name = name[:i]
	}
	return validArtifactRepository(name)
}

func validArtifactRepository(name string) bool {
	if i := strings.IndexByte(name, '/'); i > 0 && strings.ContainsAny(name[:i], ".:") || strings.HasPrefix(name, "localhost/") {
		i := strings.IndexByte(name, '/')
		u, err := url.Parse("https://" + name[:i])
		if err != nil || u.User != nil || u.Hostname() == "" || len(validation.IsDNS1123Subdomain(u.Hostname())) != 0 {
			return false
		}
		name = name[i+1:]
	}
	return artifactRepository.MatchString(name)
}

// ValidateArtifact validates the complete staging contract, including when
// admission is bypassed. Errors deliberately never include the URI or secrets.
func (s *ModelDeploymentSpec) ValidateArtifact() error {
	a := s.Model.Artifact
	if a == nil {
		return nil
	}
	if s.Model.Source != ModelSourceCustom {
		return fmt.Errorf("artifact requires model.source=custom")
	}
	if s.Provider == nil || s.Provider.Name != "vllm" {
		return fmt.Errorf("artifact requires explicit provider.name=vllm")
	}
	if s.Engine.Type != "" && s.Engine.Type != EngineTypeVLLM {
		return fmt.Errorf("artifact requires the vllm engine")
	}
	if err := s.validateArtifactCache(); err != nil {
		return err
	}
	if err := a.validateOptions(); err != nil {
		return err
	}
	u, err := a.parseURI()
	if err != nil {
		return err
	}
	if err := a.validateLocation(u); err != nil {
		return err
	}
	if err := validateArtifactScheme(u); err != nil {
		return err
	}
	return s.validateArtifactDestination(u)
}

func (s *ModelDeploymentSpec) validateArtifactCache() error {
	vol := s.ArtifactCacheVolume()
	if vol == nil || vol.ReadOnly || vol.AccessMode == corev1.ReadOnlyMany || (vol.ClaimName == "" && vol.Size == nil) {
		return fmt.Errorf("artifact requires a writable modelCache PVC")
	}
	for i := range s.Model.Storage.Volumes {
		v := &s.Model.Storage.Volumes[i]
		if v != vol && v.Purpose == VolumePurposeModelCache {
			return fmt.Errorf("artifact requires exactly one modelCache volume")
		}
	}
	if vol.MountPath != "" && (!strings.HasPrefix(vol.MountPath, "/") || !safeArtifactPath(strings.TrimPrefix(vol.MountPath, "/"))) {
		return fmt.Errorf("artifact cache mountPath must be a clean absolute directory")
	}
	return nil
}

func (a *ModelArtifactSpec) validateOptions() error {
	if a.File != "" && !safeArtifactPath(a.File) {
		return fmt.Errorf("artifact.file must be a safe relative file path")
	}
	if a.Image != "" && !validArtifactReference(a.Image) {
		return fmt.Errorf("artifact.image must be an image reference with an explicit tag or sha256 digest")
	}
	if a.ServiceAccountName != "" && len(validation.IsDNS1123Subdomain(a.ServiceAccountName)) != 0 {
		return fmt.Errorf("invalid artifact.serviceAccountName")
	}
	if a.CredentialsRef != nil {
		if len(validation.IsDNS1123Subdomain(a.CredentialsRef.Name)) != 0 {
			return fmt.Errorf("invalid artifact.credentialsRef.name")
		}
		if a.CredentialsRef.Key != "" && len(validation.IsConfigMapKey(a.CredentialsRef.Key)) != 0 {
			return fmt.Errorf("invalid artifact.credentialsRef.key")
		}
	}
	return nil
}

func (a *ModelArtifactSpec) parseURI() (*url.URL, error) {
	u, err := url.Parse(a.URI)
	if err != nil || len(a.URI) > 4096 || u.Host == "" || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(a.URI, "\\# \t\r\n") {
		return nil, fmt.Errorf("artifact.uri must be a supported URL without inline credentials, query, or fragment")
	}
	return u, nil
}

func (a *ModelArtifactSpec) validateLocation(u *url.URL) error {
	bucketRoot := u.Path == "/" && (u.Scheme == "s3" || u.Scheme == "gs")
	if u.Scheme != "oci" && u.Path != "" && !bucketRoot && !safeArtifactPath(strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), "/")) {
		return fmt.Errorf("artifact.uri contains an unsafe path")
	}
	if a.Revision != "" && (u.Scheme != "hf" || len(a.Revision) > 256 || !safeArtifactPath(a.Revision)) {
		return fmt.Errorf("artifact.revision must be a safe HF revision; it is unsupported for other schemes")
	}
	return nil
}

func validateArtifactScheme(u *url.URL) error {
	switch u.Scheme {
	case "hf":
		parts := strings.Split(u.Host+u.Path, "/")
		if len(parts) > 2 {
			return fmt.Errorf("hf artifact must identify a repository; use artifact.file for a file")
		}
		for _, part := range parts {
			if !artifactHFPart.MatchString(part) || strings.Contains(part, "..") {
				return fmt.Errorf("invalid HF repository")
			}
		}
	case "s3", "gs":
		if len(validation.IsDNS1123Subdomain(u.Host)) != 0 {
			return fmt.Errorf("invalid artifact bucket")
		}
	case "https":
		if u.Hostname() == "" || u.Path == "" || strings.HasSuffix(u.Path, "/") {
			return fmt.Errorf("HTTPS artifact must identify a single file")
		}
	case "oci":
		if !strings.Contains(u.Host+u.Path, "/") || !validArtifactReference(u.Host+u.Path) {
			return fmt.Errorf("OCI artifact must identify a registry/repository with tag or sha256 digest")
		}
	default:
		return fmt.Errorf("unsupported artifact URI scheme; use hf, s3, gs, https, or oci")
	}
	return nil
}

func (s *ModelDeploymentSpec) validateArtifactDestination(u *url.URL) error {
	a := s.Model.Artifact
	root := s.ArtifactPath()
	validID := s.Model.ID == root
	if a.File != "" {
		validID = validID || s.Model.ID == path.Join(root, a.File)
	} else if u.Scheme == "https" {
		validID = validID || s.Model.ID == path.Join(root, path.Base(u.Path))
	}
	if !validID {
		return fmt.Errorf("artifact model.id must be the cache artifacts directory or the selected file within it")
	}
	// Another mount inside the cache would hide the downloaded data at serving time.
	for _, v := range s.Model.Storage.Volumes {
		mount := v.MountPath
		if mount == "" && v.Purpose == VolumePurposeCompilationCache {
			mount = "/compilation-cache"
		}
		if v.Purpose != VolumePurposeModelCache && mount != "" && (mount == root || strings.HasPrefix(mount, root+"/") || strings.HasPrefix(root, mount+"/")) {
			return fmt.Errorf("artifact path must not overlap another storage mount")
		}
	}
	return nil
}

// ValidateArtifactUpdate prevents reusing a completed download Job for a changed
// artifact or destination. Secret contents can still be rotated in place.
func (s *ModelDeploymentSpec) ValidateArtifactUpdate(old *ModelDeploymentSpec) error {
	if !reflect.DeepEqual(s.Model.Artifact, old.Model.Artifact) {
		return fmt.Errorf("model.artifact is immutable; delete and recreate the deployment")
	}
	if s.Model.Artifact != nil && (!reflect.DeepEqual(s.ArtifactCacheVolume(), old.ArtifactCacheVolume()) || s.Model.ID != old.Model.ID || !reflect.DeepEqual(s.Secrets, old.Secrets)) {
		return fmt.Errorf("artifact destination and token reference are immutable; delete and recreate the deployment")
	}
	return nil
}
