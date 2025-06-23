package cdk8sappproxy

// Finalizer is the finalizer used by the cdk8sappproxy controller.
const (
	Finalizer = "cdk8sappproxy.addons.cluster.x-k8s.io/finalizer"
)

// Operation represents the type of operation being performed by the cdk8sappproxy controller.
const (
	OperationDeletion = "deletion"
	OperationNormal   = "normal"
)
