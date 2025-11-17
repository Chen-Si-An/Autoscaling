/*
Copyright 2025 Chen-Si-An.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

type BitnamiHelm struct {
	// +required
	ReleaseName string `json:"releaseName"`
	// +required
	ReleaseNamespace string `json:"releaseNamespace"`
	// +optional
	ControllerManaged bool `json:"controllerManaged,omitempty"`
}

type ShardTarget struct {
	// +required
	Namespace string `json:"namespace"`
	// +required
	NamePrefix string `json:"namePrefix"`
	// +required
	ServicePort int32 `json:"servicePort"`
	// +optional
	StorageClass string `json:"storageClass,omitempty"`
}

type ShardScaleBounds struct {
	// +required
	MinShards int `json:"minShards"`
	// +required
	MaxShards int `json:"maxShards"`
}

type ShardPolicy struct {
	// +required
	CpuTargetPercent int `json:"cpuTargetPercent"`
	// +required
	TolerancePercent int `json:"tolerancePercent"`
	// +required
	Window string `json:"window"`
	// +required
	CooldownSeconds int `json:"cooldownSeconds"`
}

type Prometheus struct {
	// +required
	URL string `json:"url"`
}

type SecretKeyRef struct {
	// +required
	Name string `json:"name"`
	// +required
	Key string `json:"key"`
}

type Router struct {
	// SecretRef points to a Secret containing a key "uri" with a MongoDB connection string that has admin privileges
	// +required
	SecretRef corev1.SecretReference `json:"secretRef"`
}

// MongodAutoscalerSpec defines the desired state of MongodAutoscaler
type MongodAutoscalerSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file
	// The following markers will use OpenAPI v3 schema to validate the value
	// More info: https://book.kubebuilder.io/reference/markers/crd-validation.html

	// foo is an example field of MongodAutoscaler. Edit mongodautoscaler_types.go to remove/update
	// +optional
	Foo *string `json:"foo,omitempty"`
	// +required
	Bitnami *BitnamiHelm `json:"bitnami"`
	// +required
	Target ShardTarget `json:"target"`
	// +required
	ScaleBounds ShardScaleBounds `json:"scaleBounds"`
	// +required
	Policy ShardPolicy `json:"policy"`
	// +required
	Prometheus Prometheus `json:"prometheus"`
	// +required
	Router Router `json:"router"`
}

// MongodAutoscalerStatus defines the observed state of MongodAutoscaler.
type MongodAutoscalerStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// conditions represent the current state of the MongodAutoscaler resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	LastScaleTime metav1.Time `json:"lastScaleTime,omitempty"`
	// +optional
	LastObservedCPU string `json:"lastObservedCPU,omitempty"`
	// +optional
	LastDesiredShards int32 `json:"lastDesiredShards,omitempty"`
	// +listType=set
	// +optional
	CurrentShardNames []string `json:"currentShardNames,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// MongodAutoscaler is the Schema for the mongodautoscalers API
type MongodAutoscaler struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of MongodAutoscaler
	// +required
	Spec MongodAutoscalerSpec `json:"spec"`

	// status defines the observed state of MongodAutoscaler
	// +optional
	Status MongodAutoscalerStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// MongodAutoscalerList contains a list of MongodAutoscaler
type MongodAutoscalerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MongodAutoscaler `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MongodAutoscaler{}, &MongodAutoscalerList{})
}
