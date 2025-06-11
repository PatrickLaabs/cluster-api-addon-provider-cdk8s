package cdk8sappproxy

import (
	"context"
	addonsv1alpha1 "github.com/PatrickLaabs/cluster-api-addon-provider-cdk8s/api/v1alpha1"
	"github.com/go-git/go-git/v5"
	plumbingtransport "github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"strings"
)

func (r *Reconciler) getGitAuth(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, authSecretRef *corev1.LocalObjectReference, logger logr.Logger, operation string) (*http.BasicAuth, error) {
	if authSecretRef == nil {
		return &http.BasicAuth{}, nil
	}

	logger.Info("AuthSecretRef specified, attempting to fetch secret", "secretName", authSecretRef.Name, "operation", operation)

	authSecret := &corev1.Secret{}
	secretKey := client.ObjectKey{Namespace: cdk8sAppProxy.Namespace, Name: authSecretRef.Name}
	if err := r.Get(ctx, secretKey, authSecret); err != nil {
		logger.Error(err, "Failed to get auth secret", "secretName", secretKey.String(), "operation", operation)

		return nil, err
	}

	username, okUser := authSecret.Data["username"]
	password, okPass := authSecret.Data["password"]

	if !okUser || !okPass {
		err := errors.New("auth secret missing username or password fields")
		logger.Error(err, "Invalid auth secret", "secretName", secretKey.String(), "operation", operation)

		return nil, err
	}

	logger.Info("Successfully fetched auth secret", "operation", operation)

	return &http.BasicAuth{
		Username: string(username),
		Password: string(password),
	}, nil
}

func (r *Reconciler) cloneGitRepository(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, gitSpec *addonsv1alpha1.GitRepositorySpec, tempDir string, logger logr.Logger, operation string) error {
	cloneOptions := &git.CloneOptions{
		URL:      gitSpec.URL,
		Progress: &gitProgressLogger{logger: logger.WithName("git-clone")},
	}

	// Handle authentication if needed
	if gitSpec.AuthSecretRef != nil {
		auth, err := r.getGitAuth(ctx, cdk8sAppProxy, gitSpec.AuthSecretRef, logger, operation)
		if err != nil {
			return err
		}
		cloneOptions.Auth = auth
	}

	logger.Info("Executing git clone with go-git", "url", gitSpec.URL, "targetDir", tempDir, "operation", operation)

	_, err := git.PlainCloneContext(ctx, tempDir, false, cloneOptions)
	if err != nil {
		logger.Error(err, "go-git PlainCloneContext failed", "operation", operation)
		reason := addonsv1alpha1.GitCloneFailedReason
		if errors.Is(err, plumbingtransport.ErrAuthenticationRequired) || strings.Contains(err.Error(), "authentication required") {
			reason = addonsv1alpha1.GitAuthenticationFailedReason
		}

		// Only call handleError if we have a valid cdk8sAppProxy (not during deletion cleanup)
		if cdk8sAppProxy != nil {
			return r.handleError(ctx, cdk8sAppProxy, reason, "go-git clone failed during "+operation, err)
		}

		return err
	}
	logger.Info("Successfully cloned git repository with go-git", "operation", operation)

	return nil
}
