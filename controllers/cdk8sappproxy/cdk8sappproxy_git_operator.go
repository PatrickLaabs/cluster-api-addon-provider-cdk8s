package cdk8sappproxy

import (
	"bytes"
	"context"
	addonsv1alpha1 "github.com/PatrickLaabs/cluster-api-addon-provider-cdk8s/api/v1alpha1"
	gitoperator "github.com/PatrickLaabs/cluster-api-addon-provider-cdk8s/controllers/cdk8sappproxy/git"
	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/types"
	"os"
	"path/filepath"
)

func (r *Reconciler) prepareSource(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, proxyNamespacedName types.NamespacedName, logger logr.Logger, operation string) (appSourcePath string, currentCommitHash string, err error) {
	gitImpl := &gitoperator.GitImplementer{}
	var buf bytes.Buffer
	gitSpec := cdk8sAppProxy.Spec.GitRepository

	switch {
	case cdk8sAppProxy.Spec.GitRepository != nil && cdk8sAppProxy.Spec.GitRepository.URL != "":
		logger.Info("Determining git repository URL.")

		tempDirPattern := "cdk8s-git-clone-"
		if operation == OperationDeletion {
			tempDirPattern = "cdk8s-git-delete-"
		}
		tempDir, err := os.MkdirTemp("", tempDirPattern)
		if err != nil {
			return "", "", err
		}

		retrieveCommitHash, err := gitImpl.Clone(gitSpec.URL, tempDir, &buf)
		if err != nil {
			logger.Error(err, addonsv1alpha1.GitCloneFailedCondition, "Failed to clone git repository", "operation", operation)
		}

		if operation == OperationNormal {
			currentCommitHash = retrieveCommitHash
			if currentCommitHash != "" && cdk8sAppProxy != nil {
				cdk8sAppProxy.Status.LastRemoteGitHash = currentCommitHash
				logger.Info("Updated cdk8sAppProxy.Status.LastRemoteGitHash with the latest commit hash from remote", "lastRemoteGitHash", currentCommitHash)
			}
		}

		appSourcePath = tempDir
		if gitSpec.Path != "" {
			appSourcePath = filepath.Join(tempDir, gitSpec.Path)
			logger.Info("Adjusted appSourcePath for repository subpath", "subPath", gitSpec.Path, "finalPath", appSourcePath, "operation", operation)
		}
	default:
		err := errors.New("no source specified")
		if operation == OperationNormal {
			if cdk8sAppProxy != nil {
				_ = r.updateStatusWithError(ctx, cdk8sAppProxy, addonsv1alpha1.EmptyGitRepositoryReason, "No source specified during "+operation, err, false)
			}
		} else if operation == OperationDeletion && cdk8sAppProxy != nil {
			_ = r.updateStatusWithError(ctx, cdk8sAppProxy, addonsv1alpha1.EmptyGitRepositoryReason, "No source specified, cannot determine resources to delete during "+operation, err, true)
		}

		return appSourcePath, currentCommitHash, err
	}

	return appSourcePath, currentCommitHash, nil
}
