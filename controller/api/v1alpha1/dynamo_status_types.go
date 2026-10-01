package v1alpha1

// ProviderIntentHardwareStatus reports hardware values observed in the upstream
// request. These may be supplied, discovered or inferred by Dynamo, not measured
// by Runway. Source describes their provenance relative to the user's intent.
type ProviderIntentHardwareStatus struct {
	// +optional
	// +kubebuilder:validation:MaxLength=256
	GPUSKU string `json:"gpuSku,omitempty"`
	// vramMb is the upstream per-GPU memory value in MiB. Fractional or
	// unknown values are omitted rather than rounded.
	// +optional
	// +kubebuilder:validation:Minimum=1
	VRAMMB *int64 `json:"vramMb,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=1
	NumGPUsPerNode *int32 `json:"numGpusPerNode,omitempty"`
	// +optional
	// +kubebuilder:validation:Enum=provided;discovered;mixed
	Source string `json:"source,omitempty"`
}

// ProviderIntentPlanStatus is a bounded summary of an upstream selected
// configuration or serving workload. Unknown values are omitted, not defaulted.
type ProviderIntentPlanStatus struct {
	// +kubebuilder:validation:Enum=selectedConfig;workload
	Source string `json:"source"`
	// +optional
	Engine string `json:"engine,omitempty"`
	// +optional
	ServingMode string `json:"servingMode,omitempty"`
	// workers contains at most the first 32 workers, ordered by name.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	Workers []ProviderIntentWorkerStatus `json:"workers,omitempty"`
}

// ProviderIntentWorkerStatus contains only explicitly represented worker
// settings. GPUsPerReplica is omitted when a multinode layout is ambiguous.
type ProviderIntentWorkerStatus struct {
	// +kubebuilder:validation:MaxLength=256
	Name string `json:"name"`
	// +optional
	Role string `json:"role,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=0
	Replicas *int32 `json:"replicas,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=0
	GPUsPerReplica *int32 `json:"gpusPerReplica,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=1
	TensorParallelism *int32 `json:"tensorParallelism,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=1
	PipelineParallelism *int32 `json:"pipelineParallelism,omitempty"`
}
