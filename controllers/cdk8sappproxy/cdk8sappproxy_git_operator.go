package cdk8sappproxy

import (
	"bytes"
	addonsv1alpha1 "github.com/PatrickLaabs/cluster-api-addon-provider-cdk8s/api/v1alpha1"
	gitoperator "github.com/PatrickLaabs/cluster-api-addon-provider-cdk8s/controllers/cdk8sappproxy/git"
	"github.com/go-logr/logr"
	"path/filepath"
)

func (r *Reconciler) prepareSource(cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, logger logr.Logger, operation string) (appSourcePath string, currentCommitHash string, err error) {
	gitImpl := &gitoperator.GitImplementer{}
	var buf bytes.Buffer
	gitSpec := cdk8sAppProxy.Spec.GitRepository

	if cdk8sAppProxy.Spec.GitRepository != nil && cdk8sAppProxy.Spec.GitRepository.URL != "" {

		directory, err := gitImpl.Clone(gitSpec.URL, &buf)
		if err != nil {
			logger.Error(err, addonsv1alpha1.GitCloneFailedCondition, "Failed to clone git repository")
		}

		retrieveCommitHash, err := gitImpl.Hash(directory, cdk8sAppProxy.Spec.GitRepository.Reference)
		if err != nil {
			logger.Error(err, addonsv1alpha1.GitHashFailureReason, "Failed to get local git hash")
		}

		currentCommitHash = retrieveCommitHash
		if currentCommitHash != "" && cdk8sAppProxy != nil {
			cdk8sAppProxy.Status.LastRemoteGitHash = currentCommitHash
			logger.Info("Updated cdk8sAppProxy.Status.LastRemoteGitHash with the latest commit hash from remote", "lastRemoteGitHash", currentCommitHash)
		}

		if gitSpec.Path != "" {
			appSourcePath = filepath.Join(directory, gitSpec.Path)
			logger.Info("Adjusted appSourcePath for repository subpath", "subPath", gitSpec.Path, "finalPath", appSourcePath, "operation", operation)
		}

		return appSourcePath, currentCommitHash, err
	}

	return appSourcePath, currentCommitHash, nil
}
