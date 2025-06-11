package cdk8sappproxy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	addonsv1alpha1 "github.com/PatrickLaabs/cluster-api-addon-provider-cdk8s/api/v1alpha1"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	plumbingtransport "github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
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

		// Only call updateStatusWithError if we have a valid cdk8sAppProxy (not during deletion cleanup)
		if cdk8sAppProxy != nil {
			// Determine if we should remove finalizer based on operation
			removeFinalizer := operation == OperationDeletion

			return r.updateStatusWithError(ctx, cdk8sAppProxy, reason, "go-git clone failed during "+operation, err, removeFinalizer)
		}

		return err
	}
	logger.Info("Successfully cloned git repository with go-git", "operation", operation)

	return nil
}

// pollGitRepository periodically checks the remote git repository for changes.
func (r *Reconciler) pollGitRepository(ctx context.Context, proxyName types.NamespacedName) {
	logger := log.FromContext(ctx).WithValues("cdk8sappproxy", proxyName.String(), "goroutine", "pollGitRepository")
	logger.Info("Starting git repository polling loop")

	ticker := time.NewTicker(gitPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := r.pollGitRepositoryOnce(ctx, proxyName, logger); err != nil {
				logger.Error(err, "Error during git repository polling")
			}
		case <-ctx.Done():
			logger.Info("Stopping git repository polling loop due to context cancellation.")

			return
		}
	}
}

func (r *Reconciler) pollGitRepositoryOnce(ctx context.Context, proxyName types.NamespacedName, logger logr.Logger) error {
	logger.Info("Polling git repository for changes")

	cdk8sAppProxy, err := r.getCdk8sAppProxyForPolling(ctx, proxyName)
	if err != nil {
		return err
	}

	if cdk8sAppProxy == nil {
		logger.Info("Cdk8sAppProxy resource not found, stopping polling.")

		return errors.New("resource not found")
	}

	if !r.isGitRepositoryConfigured(cdk8sAppProxy, logger) {
		return errors.New("git repository not configured")
	}

	gitSpec := cdk8sAppProxy.Spec.GitRepository
	refName := r.determineGitReference(gitSpec, logger)

	remoteCommitHash, err := r.fetchRemoteCommitHash(ctx, cdk8sAppProxy, gitSpec, refName, logger)
	if err != nil {
		return err
	}

	return r.handleGitRepositoryChange(ctx, proxyName, cdk8sAppProxy, remoteCommitHash, logger)
}

func (r *Reconciler) prepareSourceForDeletion(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, logger logr.Logger) (string, func(), error) {
	if cdk8sAppProxy.Spec.GitRepository != nil && cdk8sAppProxy.Spec.GitRepository.URL != "" {
		return r.prepareGitSourceForDeletion(ctx, cdk8sAppProxy, logger)
	} else if cdk8sAppProxy.Spec.LocalPath != "" {
		return cdk8sAppProxy.Spec.LocalPath, func() {}, nil
	}

	err := errors.New("neither GitRepository nor LocalPath specified, cannot determine resources to delete")
	logger.Info(err.Error())
	_ = r.updateStatusWithError(ctx, cdk8sAppProxy, addonsv1alpha1.SourceNotSpecifiedReason, "Cannot determine resources to delete during deletion", err, true)

	return "", func() {}, err
}

func (r *Reconciler) prepareGitSourceForDeletion(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, logger logr.Logger) (string, func(), error) {
	gitSpec := cdk8sAppProxy.Spec.GitRepository
	logger.Info("Using GitRepository source for deletion logic", "url", gitSpec.URL, "reference", gitSpec.Reference, "path", gitSpec.Path)

	tempDir, err := os.MkdirTemp("", "cdk8s-git-delete-")
	if err != nil {
		logger.Error(err, "failed to create temp dir for git clone during deletion")

		return "", func() {}, err
	}

	cleanupFunc := func() {
		logger.Info("Removing temporary clone directory for deletion", "tempDir", tempDir)
		if err := os.RemoveAll(tempDir); err != nil {
			logger.Error(err, "Failed to remove temporary clone directory for deletion", "tempDir", tempDir)
		}
	}

	logger.Info("Created temporary directory for clone during deletion", "tempDir", tempDir)

	if err := r.cloneGitRepository(ctx, nil, gitSpec, tempDir, logger, OperationDeletion); err != nil {
		cleanupFunc()

		return "", func() {}, err
	}

	if err := r.checkoutGitReference(ctx, nil, gitSpec, tempDir, logger, OperationDeletion); err != nil {
		cleanupFunc()

		return "", func() {}, err
	}

	appSourcePath := tempDir
	if gitSpec.Path != "" {
		appSourcePath = filepath.Join(tempDir, gitSpec.Path)
		logger.Info("Adjusted appSourcePath for deletion", "subPath", gitSpec.Path, "finalPath", appSourcePath)
	}

	return appSourcePath, cleanupFunc, nil
}

func (r *Reconciler) findRemoteCommitHash(refs []*plumbing.Reference, refName plumbing.ReferenceName, logger logr.Logger) (string, error) {
	var remoteCommitHash string
	foundRef := false

	for _, ref := range refs {
		if ref.Name() == refName {
			remoteCommitHash = ref.Hash().String()
			foundRef = true
			logger.Info("Found matching reference in LsRemote output", "refName", refName.String(), "remoteCommitHash", remoteCommitHash)

			break
		}
	}

	if !foundRef {
		foundRef, remoteCommitHash = r.tryFindDefaultBranch(refs, refName, logger)
	}

	if !foundRef {
		logger.Info("Specified reference not found in LsRemote output.", "refName", refName.String())

		return "", errors.New("reference not found")
	}

	if remoteCommitHash == "" {
		logger.Info("Remote commit hash is empty after LsRemote, skipping update check.")

		return "", errors.New("empty commit hash")
	}

	return remoteCommitHash, nil
}

func (r *Reconciler) tryFindDefaultBranch(refs []*plumbing.Reference, refName plumbing.ReferenceName, logger logr.Logger) (bool, string) {
	if refName != plumbing.HEAD {
		return false, ""
	}

	logger.Info("HEAD reference not explicitly found, searching for default branches like main/master.")
	defaultBranches := []plumbing.ReferenceName{
		plumbing.NewBranchReferenceName("main"),
		plumbing.NewBranchReferenceName("master"),
	}

	for _, defaultBranchRef := range defaultBranches {
		for _, ref := range refs {
			if ref.Name() == defaultBranchRef {
				remoteCommitHash := ref.Hash().String()
				logger.Info("Found default branch reference", "refName", defaultBranchRef.String(), "remoteCommitHash", remoteCommitHash)

				return true, remoteCommitHash
			}
		}
	}

	return false, ""
}

func (r *Reconciler) handleGitRepositoryChange(ctx context.Context, proxyName types.NamespacedName, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, remoteCommitHash string, logger logr.Logger) error {
	if cdk8sAppProxy.Status.LastRemoteGitHash == remoteCommitHash {
		logger.Info("No change detected in remote git repository.", "currentRemoteHash", remoteCommitHash)

		return nil
	}

	logger.Info("Detected change in remote git repository", "oldHash", cdk8sAppProxy.Status.LastRemoteGitHash, "newHash", remoteCommitHash)

	if err := r.updateRemoteGitHashStatus(ctx, proxyName, remoteCommitHash, logger); err != nil {
		return err
	}

	return r.triggerReconciliation(ctx, proxyName, logger)
}

func (r *Reconciler) isGitRepositoryConfigured(cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, logger logr.Logger) bool {
	if cdk8sAppProxy.Spec.GitRepository == nil || cdk8sAppProxy.Spec.GitRepository.URL == "" {
		logger.Info("GitRepository not configured for this Cdk8sAppProxy, stopping polling.")

		return false
	}

	return true
}

func (r *Reconciler) determineGitReference(gitSpec *addonsv1alpha1.GitRepositorySpec, logger logr.Logger) plumbing.ReferenceName {
	refName := plumbing.HEAD
	if gitSpec.Reference == "" {
		return refName
	}

	switch {
	case plumbing.IsHash(gitSpec.Reference):
		logger.Info("Polling a specific commit hash is not actively supported. The poller will check if the remote still has this hash, but it won't detect 'new' commits beyond this specific one. If you want to track a branch, please specify a branch name.", "reference", gitSpec.Reference)
		refName = plumbing.ReferenceName(gitSpec.Reference)
	case strings.HasPrefix(gitSpec.Reference, "refs/"):
		refName = plumbing.ReferenceName(gitSpec.Reference)
	case strings.Contains(gitSpec.Reference, "/"):
		refName = plumbing.ReferenceName(gitSpec.Reference)
	default:
		logger.Info("Assuming Git reference is a branch name, prepending 'refs/heads/' for LsRemote.", "reference", gitSpec.Reference)
		refName = plumbing.NewBranchReferenceName(gitSpec.Reference)
	}

	return refName
}

func (r *Reconciler) fetchRemoteCommitHash(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, gitSpec *addonsv1alpha1.GitRepositorySpec, refName plumbing.ReferenceName, logger logr.Logger) (string, error) {
	logger.Info("Attempting to LsRemote", "url", gitSpec.URL, "refName", refName.String())

	rem := git.NewRemote(memory.NewStorage(), &config.RemoteConfig{
		URLs: []string{gitSpec.URL},
	})

	auth, err := r.getGitAuth(ctx, cdk8sAppProxy, gitSpec.AuthSecretRef, logger, "fetchRemoteCommitHash")
	if err != nil {
		return "", err
	}

	refs, err := rem.ListContext(ctx, &git.ListOptions{Auth: auth})
	if err != nil {
		logger.Error(err, "Failed to LsRemote from git repository")
		if errors.Is(err, plumbingtransport.ErrAuthenticationRequired) || strings.Contains(err.Error(), "authentication required") {
			logger.Info("Authentication failed for LsRemote. Please check credentials.")
		}

		return "", err
	}

	return r.findRemoteCommitHash(refs, refName, logger)
}

func (r *Reconciler) manageGitPollerLifecycle(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, proxyNamespacedName types.NamespacedName, logger logr.Logger) {
	if cdk8sAppProxy.Spec.GitRepository != nil && cdk8sAppProxy.Spec.GitRepository.URL != "" {
		r.startGitPollerIfNeeded(ctx, proxyNamespacedName, logger)
	} else {
		r.stopGitPollerIfNotNeeded(proxyNamespacedName, logger)
	}
}

func (r *Reconciler) startGitPollerIfNeeded(ctx context.Context, proxyNamespacedName types.NamespacedName, logger logr.Logger) {
	if _, pollerExists := r.activeGitPollers[proxyNamespacedName]; !pollerExists {
		logger.Info("Starting new git poller.")
		pollCtx, cancelPoll := context.WithCancel(ctx)
		r.activeGitPollers[proxyNamespacedName] = cancelPoll
		go r.pollGitRepository(pollCtx, proxyNamespacedName)
	} else {
		logger.Info("Git poller already active.")
	}
}

func (r *Reconciler) stopGitPollerIfNotNeeded(proxyNamespacedName types.NamespacedName, logger logr.Logger) {
	if cancel, pollerExists := r.activeGitPollers[proxyNamespacedName]; pollerExists {
		logger.Info("GitRepository is not configured, ensuring poller is stopped.")
		cancel()
		delete(r.activeGitPollers, proxyNamespacedName)
	}
}

// You already have this function, but let's also create a helper for explicit stopping.
func (r *Reconciler) stopGitPoller(proxyNamespacedName types.NamespacedName, logger logr.Logger) {
	if cancel, pollerExists := r.activeGitPollers[proxyNamespacedName]; pollerExists {
		logger.Info("Stopping git poller.")
		cancel()
		delete(r.activeGitPollers, proxyNamespacedName)
	} else {
		logger.Info("No active git poller found to stop.")
	}
}
