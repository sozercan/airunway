package v1alpha1

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
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
