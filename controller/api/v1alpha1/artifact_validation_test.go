package v1alpha1

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func artifactSpec() ModelDeploymentSpec {
	return ModelDeploymentSpec{
		Model:    ModelSpec{Source: ModelSourceCustom, ID: "/model-cache/artifacts", Artifact: &ModelArtifactSpec{URI: "hf://org/model"}, Storage: &StorageSpec{Volumes: []StorageVolume{{Name: "weights", ClaimName: "weights", Purpose: VolumePurposeModelCache}}}},
		Provider: &ProviderSpec{Name: "vllm"},
	}
}

func TestValidateArtifact(t *testing.T) {
	for _, uri := range []string{"hf://org/model", "hf://gpt2", "s3://bucket/prefix/", "gs://bucket/prefix", "https://example.com/model.gguf", "https://a.blob.core.windows.net/models/model.gguf", "oci://registry.example.com/org/model:v1", "oci://localhost:5000/model@sha256:" + strings.Repeat("a", 64)} {
		t.Run(uri, func(t *testing.T) {
			s := artifactSpec()
			s.Model.Artifact.URI = uri
			if err := s.ValidateArtifact(); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, uri := range []string{"", "file:///tmp/model", "http://example.com/file", "ftp://example.com/file", "https://user:secret@example.com/file", "https://example.com/file?sig=secret", "https://example.com/file#secret", "https://example.com/file?", "hf://org/model/file", "hf://org/../file", "s3://bucket/prefix/../file", "gs://bucket/%2e%2e/file", "https://example.com/%252e%252e/file", "https://example.com/", "oci://registry.example.com/model", "oci://registry.example.com/model@sha256:bad", "oci://registry.example.com/../model:v1"} {
		t.Run("bad "+uri, func(t *testing.T) {
			s := artifactSpec()
			s.Model.Artifact.URI = uri
			if err := s.ValidateArtifact(); err == nil {
				t.Fatal("accepted invalid source")
			} else if strings.Contains(err.Error(), "secret") {
				t.Fatal("error disclosed URL credentials")
			}
		})
	}
	cases := map[string]func(*ModelDeploymentSpec){
		"provider auto":   func(s *ModelDeploymentSpec) { s.Provider = nil },
		"provider dynamo": func(s *ModelDeploymentSpec) { s.Provider.Name = "dynamo" },
		"source":          func(s *ModelDeploymentSpec) { s.Model.Source = ModelSourceHuggingFace },
		"engine":          func(s *ModelDeploymentSpec) { s.Engine.Type = EngineTypeSGLang },
		"missing cache":   func(s *ModelDeploymentSpec) { s.Model.Storage = nil },
		"read only":       func(s *ModelDeploymentSpec) { s.Model.Storage.Volumes[0].ReadOnly = true },
		"access mode":     func(s *ModelDeploymentSpec) { s.Model.Storage.Volumes[0].AccessMode = corev1.ReadOnlyMany },
		"missing claim":   func(s *ModelDeploymentSpec) { s.Model.Storage.Volumes[0].ClaimName = "" },
		"duplicate cache": func(s *ModelDeploymentSpec) {
			s.Model.Storage.Volumes = append(s.Model.Storage.Volumes, s.Model.Storage.Volumes[0])
		},
		"unsafe mount": func(s *ModelDeploymentSpec) { s.Model.Storage.Volumes[0].MountPath = "/cache/../models" },
		"hidden files": func(s *ModelDeploymentSpec) {
			s.Model.Storage.Volumes = append(s.Model.Storage.Volumes, StorageVolume{MountPath: "/model-cache/artifacts/config.json"})
		},
		"implicit cache overlap": func(s *ModelDeploymentSpec) {
			s.Model.Storage.Volumes[0].MountPath = "/compilation-cache"
			s.Model.ID = "/compilation-cache/artifacts"
			s.Model.Storage.Volumes = append(s.Model.Storage.Volumes, StorageVolume{Purpose: VolumePurposeCompilationCache})
		},
		"unsafe file":     func(s *ModelDeploymentSpec) { s.Model.Artifact.File = "../model" },
		"absolute file":   func(s *ModelDeploymentSpec) { s.Model.Artifact.File = "/model" },
		"nul file":        func(s *ModelDeploymentSpec) { s.Model.Artifact.File = "bad\x00file" },
		"unsafe revision": func(s *ModelDeploymentSpec) { s.Model.Artifact.Revision = "../main" },
		"unsupported revision": func(s *ModelDeploymentSpec) {
			s.Model.Artifact.URI = "s3://bucket/prefix"
			s.Model.Artifact.Revision = "v1"
		},
		"invalid image": func(s *ModelDeploymentSpec) { s.Model.Artifact.Image = "https://example.com/image" },
		"invalid secret": func(s *ModelDeploymentSpec) {
			s.Model.Artifact.CredentialsRef = &ArtifactCredentialsRef{Name: "Bad_Secret"}
		},
		"invalid key": func(s *ModelDeploymentSpec) {
			s.Model.Artifact.CredentialsRef = &ArtifactCredentialsRef{Name: "secret", Key: "../key"}
		},
		"invalid account":  func(s *ModelDeploymentSpec) { s.Model.Artifact.ServiceAccountName = "bad/account" },
		"wrong model path": func(s *ModelDeploymentSpec) { s.Model.ID = "org/model" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := artifactSpec()
			mutate(&s)
			if err := s.ValidateArtifact(); err == nil {
				t.Fatal("accepted invalid contract")
			}
		})
	}
	s := artifactSpec()
	s.Model.Artifact.File = "quantized/model.gguf"
	s.Model.Artifact.Revision = "refs/pr/1"
	s.Model.ID += "/quantized/model.gguf"
	s.Model.Artifact.Image = "ghcr.io/org/downloader:v1"
	s.Model.Artifact.CredentialsRef = &ArtifactCredentialsRef{Name: "download-auth"}
	s.Model.Artifact.ServiceAccountName = "model-reader"
	if err := s.ValidateArtifact(); err != nil {
		t.Fatal(err)
	}
	s.Model.Artifact = nil
	if err := s.ValidateArtifact(); err != nil {
		t.Fatal("non-artifact spec changed", err)
	}
}

func TestValidateArtifactBuckets(t *testing.T) {
	maxGCSBucket := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 30)
	for _, tt := range []struct {
		name  string
		uri   string
		valid bool
	}{
		{name: "GCS underscore prefix", uri: "gs://model_cache/weights", valid: true},
		{name: "GCS underscore root", uri: "gs://model_cache", valid: true},
		{name: "GCS underscore slash root", uri: "gs://model_cache/", valid: true},
		{name: "GCS repeated underscores", uri: "gs://model__cache/weights", valid: true},
		{name: "GCS minimum length", uri: "gs://0_1/weights", valid: true},
		{name: "GCS dotted underscore", uri: "gs://model_cache.example.com/weights", valid: true},
		{name: "GCS maximum component", uri: "gs://" + strings.Repeat("a", 31) + "_" + strings.Repeat("b", 31) + "/weights", valid: true},
		{name: "GCS maximum dotted length", uri: "gs://" + maxGCSBucket + "/weights", valid: true},
		{name: "GCS too short", uri: "gs://ab/weights"},
		{name: "GCS too long without dots", uri: "gs://" + strings.Repeat("a", 64) + "/weights"},
		{name: "GCS too long with dots", uri: "gs://" + maxGCSBucket + "d/weights"},
		{name: "GCS component too long", uri: "gs://" + strings.Repeat("a", 64) + ".example.com/weights"},
		{name: "GCS leading underscore", uri: "gs://_model_cache/weights"},
		{name: "GCS trailing underscore", uri: "gs://model_cache_/weights"},
		{name: "GCS leading dash", uri: "gs://-model_cache/weights"},
		{name: "GCS trailing dot", uri: "gs://model_cache./weights"},
		{name: "GCS uppercase", uri: "gs://Model_cache/weights"},
		{name: "GCS invalid character", uri: "gs://model~cache/weights"},
		{name: "GCS port", uri: "gs://model_cache:443/weights"},
		{name: "GCS empty component", uri: "gs://model..cache/weights"},
		{name: "GCS multiple empty components", uri: "gs://model...cache/weights"},
		{name: "GCS IPv4 address", uri: "gs://192.168.0.1/weights"},
		{name: "GCS zero IPv4 address", uri: "gs://0.0.0.0/weights"},
		{name: "GCS max IPv4 address", uri: "gs://255.255.255.255/weights"},
		{name: "GCS dotted name not an IP", uri: "gs://192.168.0.1.cache/weights", valid: true},
		{name: "S3 one character", uri: "s3://a/weights"},
		{name: "S3 two characters", uri: "s3://ab/weights"},
		{name: "S3 minimum length", uri: "s3://abc/weights", valid: true},
		{name: "S3 maximum length", uri: "s3://" + strings.Repeat("a", 63) + "/weights", valid: true},
		{name: "S3 over maximum length", uri: "s3://" + strings.Repeat("a", 64) + "/weights"},
		{name: "S3 IPv4 address", uri: "s3://192.168.0.1/weights"},
		{name: "S3 dash unchanged", uri: "s3://model-cache/weights", valid: true},
		{name: "S3 dots unchanged", uri: "s3://model.cache/weights", valid: true},
		{name: "S3 underscore rejected", uri: "s3://model_cache/weights"},
		{name: "S3 uppercase rejected", uri: "s3://Model-cache/weights"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := artifactSpec()
			s.Model.Artifact.URI = tt.uri
			if err := s.ValidateArtifact(); (err == nil) != tt.valid {
				t.Fatalf("ValidateArtifact() error = %v, want valid = %t", err, tt.valid)
			}
		})
	}
}

func TestValidateArtifactUpdate(t *testing.T) {
	old := artifactSpec()
	for name, mutate := range map[string]func(*ModelDeploymentSpec){
		"uri":         func(s *ModelDeploymentSpec) { s.Model.Artifact.URI = "s3://other/model" },
		"file":        func(s *ModelDeploymentSpec) { s.Model.Artifact.File = "model.gguf" },
		"revision":    func(s *ModelDeploymentSpec) { s.Model.Artifact.Revision = "v2" },
		"image":       func(s *ModelDeploymentSpec) { s.Model.Artifact.Image = "image:v2" },
		"identity":    func(s *ModelDeploymentSpec) { s.Model.Artifact.ServiceAccountName = "new" },
		"credentials": func(s *ModelDeploymentSpec) { s.Model.Artifact.CredentialsRef = &ArtifactCredentialsRef{Name: "new"} },
		"removed":     func(s *ModelDeploymentSpec) { s.Model.Artifact = nil },
		"cache":       func(s *ModelDeploymentSpec) { s.Model.Storage.Volumes[0].ClaimName = "other" },
		"path":        func(s *ModelDeploymentSpec) { s.Model.ID = "/model-cache/artifacts/other" },
	} {
		t.Run(name, func(t *testing.T) {
			s := old.DeepCopy()
			mutate(s)
			if err := s.ValidateArtifactUpdate(&old); err == nil {
				t.Fatal("accepted identity change")
			}
		})
	}
	s := old.DeepCopy()
	if err := s.ValidateArtifactUpdate(&old); err != nil {
		t.Fatal(err)
	}
	before := old.DeepCopy()
	before.Model.Artifact = nil
	if err := old.ValidateArtifactUpdate(before); err == nil {
		t.Fatal("accepted artifact addition")
	}
}

func TestArtifactURIPortRange(t *testing.T) {
	for _, scheme := range []string{"https", "oci"} {
		for _, port := range []string{"0", "70000", "999999999999999999999999999", ""} {
			uri := scheme + "://registry.example.test:" + port + "/model:v1"
			artifact := &ModelArtifactSpec{URI: uri}
			if _, err := artifact.parseURI(); err == nil {
				t.Fatalf("accepted invalid port in %s", uri)
			}
		}
		for _, port := range []string{"1", "443", "65535"} {
			uri := scheme + "://registry.example.test:" + port + "/model:v1"
			artifact := &ModelArtifactSpec{URI: uri}
			if _, err := artifact.parseURI(); err != nil {
				t.Fatalf("valid port rejected in %s: %v", uri, err)
			}
		}
	}
}

func TestArtifactDownloaderImagePortRange(t *testing.T) {
	for _, suffix := range []string{":v1", "@sha256:" + strings.Repeat("a", 64)} {
		for _, port := range []string{"0", "65536", "70000", "999999999999999999999999999", "", "-1", "abc"} {
			s := artifactSpec()
			s.Model.Artifact.Image = "registry.example:" + port + "/downloader" + suffix
			if err := s.ValidateArtifact(); err == nil {
				t.Errorf("accepted downloader image with invalid port %q", port)
			}
		}
		for _, port := range []string{"1", "443", "65535"} {
			s := artifactSpec()
			s.Model.Artifact.Image = "registry.example:" + port + "/downloader" + suffix
			if err := s.ValidateArtifact(); err != nil {
				t.Errorf("rejected downloader image with valid port %q: %v", port, err)
			}
		}
	}
}

func TestArtifactCacheQuantityUpdate(t *testing.T) {
	old := artifactSpec()
	size := resource.MustParse("1Gi")
	old.Model.Storage.Volumes[0].Size = &size
	for _, quantity := range []string{"1Gi", "1024Mi", "1073741824"} {
		s := old.DeepCopy()
		size := resource.MustParse(quantity)
		s.Model.Storage.Volumes[0].Size = &size
		if err := s.ValidateArtifactUpdate(&old); err != nil {
			t.Errorf("equivalent quantity %s rejected: %v", quantity, err)
		}
	}
	for _, quantity := range []string{"2Gi", "1G", "0"} {
		s := old.DeepCopy()
		size := resource.MustParse(quantity)
		s.Model.Storage.Volumes[0].Size = &size
		if err := s.ValidateArtifactUpdate(&old); err == nil {
			t.Errorf("changed quantity %s accepted", quantity)
		}
	}
	s := old.DeepCopy()
	s.Model.Storage.Volumes[0].Size = nil
	if err := s.ValidateArtifactUpdate(&old); err == nil {
		t.Error("removed cache quantity accepted")
	}
}

func TestArtifactDestinationMountOverlap(t *testing.T) {
	for _, mount := range []string{"/", "/model-cache", "/model-cache/artifacts", "/model-cache/artifacts/config.json", "/model-cache-other", "/other"} {
		s := artifactSpec()
		s.Model.Storage.Volumes = append(s.Model.Storage.Volumes, StorageVolume{Name: "other", ClaimName: "other", MountPath: mount, Purpose: VolumePurposeCompilationCache})
		valid := mount == "/model-cache-other" || mount == "/other"
		if err := s.ValidateArtifact(); (err == nil) != valid {
			t.Errorf("mount %q: error = %v, want valid = %t", mount, err, valid)
		}
	}
}
