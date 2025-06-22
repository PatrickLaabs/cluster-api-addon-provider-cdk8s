package cdk8sappproxy

import "time"

// Finalizer is the finalizer used by the cdk8sappproxy controller.
const (
	Finalizer = "cdk8sappproxy.addons.cluster.x-k8s.io/finalizer"
)

// Operation represents the type of operation being performed by the cdk8sappproxy controller.
const (
	OperationDeletion   = "deletion"
	OperationNormal     = "normal"
	OperationPolling    = "polling"
	OperationFindFiles  = "findFiles"
	OperationSynthesize = "synthesize"
	OperationApply      = "apply"
	OperationNpmInstall = "npmInstall"
)

// gitPollInterval is the interval at which the cdk8sappproxy controller polls the git repository for changes.
const (
	gitPollInterval      = 1 * time.Minute
	consecutiveErrors    = 0
	maxConsecutiveErrors = 5
)
