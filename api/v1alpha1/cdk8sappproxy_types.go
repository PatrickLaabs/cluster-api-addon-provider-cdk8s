/*
Copyright 2023 The Kubernetes Authors.

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
	corev1 "k8s.io/api/core/v1" // Ensure this import
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
)

// GitRepositorySpec defines the desired state of a Git repository source.
type GitRepositorySpec struct {
	// URL is the git repository URL.
	// +kubebuilder:validation:Required
	URL string `json:"url"`

	// Reference is the git reference (branch, tag, or commit).
	// +kubebuilder:validation:Optional
	Reference string `json:"reference,omitempty"`

	// Path is the path within the repository where the cdk8s application is located.
	// Defaults to the root of the repository.
	// +kubebuilder:validation:Optional
	Path string `json:"path,omitempty"`

	// AuthSecretRef is a reference to a Secret in the same namespace
	// containing authentication credentials for the Git repository.
	// The secret must contain 'username' and 'password' fields.
	// +kubebuilder:validation:Optional
	AuthSecretRef *corev1.LocalObjectReference `json:"authSecretRef,omitempty"` // New field
}

// Cdk8sAppProxySpec defines the desired state of Cdk8sAppProxy.
type Cdk8sAppProxySpec struct {
	// LocalPath is the local filesystem path to the cdk8s app.
	// One of LocalPath or GitRepository must be specified.
	// +kubebuilder:validation:Optional
	LocalPath string `json:"localPath,omitempty"`

	// GitRepository specifies the Git repository for the cdk8s app.
	// One of LocalPath or GitRepository must be specified.
	// +kubebuilder:validation:Optional
	GitRepository *GitRepositorySpec `json:"gitRepository,omitempty"`

	// Values is a string containing the values to be passed to the cdk8s app
	// +kubebuilder:validation:Optional
	Values string `json:"values,omitempty"`

	// ClusterSelector selects the clusters to deploy the cdk8s app to.
	// +kubebuilder:validation:Required
	ClusterSelector metav1.LabelSelector `json:"clusterSelector"`
}

// Cdk8sAppProxyStatus defines the observed state of Cdk8sAppProxy.
type Cdk8sAppProxyStatus struct {
	// Conditions defines the current state of the Cdk8sAppProxy.
	// +optional
	Conditions clusterv1.Conditions `json:"conditions,omitempty"`

	// ObservedGeneration is the last generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// LastProcessedGitHash stores the commit hash of the last successfully reconciled Git state.
	// +optional
	LastProcessedGitHash string `json:"lastProcessedGitHash,omitempty"`

	// LastRemoteGitHash is the last commit hash fetched from the remote git repository.
	// +optional
	LastRemoteGitHash string `json:"lastRemoteGitHash,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// Cdk8sAppProxy is the Schema for the cdk8sappproxies API.
type Cdk8sAppProxy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   Cdk8sAppProxySpec   `json:"spec,omitempty"`
	Status Cdk8sAppProxyStatus `json:"status,omitempty"`
}

// GetConditions returns the list of conditions for an Cdk8sAppProxy API object.
func (c *Cdk8sAppProxy) GetConditions() clusterv1.Conditions {
	return c.Status.Conditions
}

// SetConditions sets the conditions on an Cdk8sAppProxy API object.
func (c *Cdk8sAppProxy) SetConditions(conditions clusterv1.Conditions) {
	c.Status.Conditions = conditions
}

//+kubebuilder:object:root=true

// Cdk8sAppProxyList contains a list of Cdk8sAppProxy.
type Cdk8sAppProxyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Cdk8sAppProxy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Cdk8sAppProxy{}, &Cdk8sAppProxyList{})
}
