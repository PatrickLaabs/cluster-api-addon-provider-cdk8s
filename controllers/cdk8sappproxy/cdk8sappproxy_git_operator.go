package cdk8sappproxy

import (
	"bytes"
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
	cdk8sAppProxy, err := r.getCdk8sAppProxyForPolling(ctx, proxyName)
	if err != nil {
		return err
	}

	if cdk8sAppProxy == nil {
		logger.Info("Cdk8sAppProxy resource not found, stopping polling.")

		return errors.New("resource not found")
	}

	gitSpec := cdk8sAppProxy.Spec.GitRepository
	refName := r.determineGitReference(gitSpec)

	remoteCommitHash, err := r.fetchRemoteCommitHash(ctx, cdk8sAppProxy, gitSpec, refName, logger)
	if err != nil {
		return err
	}

	return r.handleGitRepositoryChange(ctx, proxyName, cdk8sAppProxy, remoteCommitHash, logger)
}

func (r *Reconciler) prepareSourceForDeletion(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, logger logr.Logger) (string, func(), error) {
	if cdk8sAppProxy.Spec.GitRepository != nil && cdk8sAppProxy.Spec.GitRepository.URL != "" {
		return r.prepareGitSourceForDeletion(ctx, cdk8sAppProxy, logger)
	}

	err := errors.New("GitRepository not specified, cannot determine resources to delete")
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
	// First, try to find the exact reference
	for _, ref := range refs {
		if ref.Name() == refName {
			remoteCommitHash := ref.Hash().String()

			return remoteCommitHash, nil
		}
	}
	logger.Error(nil, "Specified reference not found in remote repository", "refName", refName.String())

	return "", errors.Errorf("reference '%s' not found in remote repository", refName.String())
}

func (r *Reconciler) determineGitReference(gitSpec *addonsv1alpha1.GitRepositorySpec) plumbing.ReferenceName {
	if gitSpec.Reference == "" {
		return plumbing.NewBranchReferenceName("main")
	}

	switch {
	case plumbing.IsHash(gitSpec.Reference):

		return plumbing.ReferenceName(gitSpec.Reference)
	case strings.HasPrefix(gitSpec.Reference, "refs/"):
		return plumbing.ReferenceName(gitSpec.Reference)
	case strings.Contains(gitSpec.Reference, "/"):
		return plumbing.ReferenceName(gitSpec.Reference)
	default:
		return plumbing.NewBranchReferenceName(gitSpec.Reference)
	}
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
		r.stopGitPoller(proxyNamespacedName, logger)
	}
}

func (r *Reconciler) startGitPollerIfNeeded(ctx context.Context, proxyNamespacedName types.NamespacedName, logger logr.Logger) {
	if _, pollerExists := r.ActiveGitPollers[proxyNamespacedName]; !pollerExists {
		logger.Info("Starting new git poller.")
		pollCtx, cancelPoll := context.WithCancel(ctx)
		r.ActiveGitPollers[proxyNamespacedName] = cancelPoll
		go r.pollGitRepository(pollCtx, proxyNamespacedName)
	} else {
		logger.Info("Git poller already active.")
	}
}

func (r *Reconciler) stopGitPoller(proxyNamespacedName types.NamespacedName, logger logr.Logger) {
	if cancel, pollerExists := r.ActiveGitPollers[proxyNamespacedName]; pollerExists {
		logger.Info("Stopping git poller.")
		cancel()
		delete(r.ActiveGitPollers, proxyNamespacedName)
	} else {
		logger.Info("No active git poller found to stop.")
	}
}

func (r *Reconciler) updateRemoteGitHashStatus(ctx context.Context, proxyName types.NamespacedName, remoteCommitHash string, logger logr.Logger) error {
	latestCdk8sAppProxy := &addonsv1alpha1.Cdk8sAppProxy{}
	if err := r.Get(ctx, proxyName, latestCdk8sAppProxy); err != nil {
		logger.Error(err, "Failed to get latest Cdk8sAppProxy before status update")

		return err
	}

	latestCdk8sAppProxy.Status.LastRemoteGitHash = remoteCommitHash
	if err := r.Status().Update(ctx, latestCdk8sAppProxy); err != nil {
		logger.Error(err, "Failed to update Cdk8sAppProxy status with new remote hash")

		return err
	}

	logger.Info("Successfully updated status with new remote hash. Now triggering reconciliation.")

	return nil
}

func (r *Reconciler) prepareSource(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, proxyNamespacedName types.NamespacedName, logger logr.Logger) (string, string, func(), error) {
	var appSourcePath string
	var currentCommitHash string
	var cleanupFunc func()

	switch {
	case cdk8sAppProxy.Spec.GitRepository != nil && cdk8sAppProxy.Spec.GitRepository.URL != "":
		path, hash, cleanup, err := r.prepareGitSource(ctx, cdk8sAppProxy, logger)
		if err != nil {
			return "", "", nil, err
		}
		appSourcePath = path
		currentCommitHash = hash
		cleanupFunc = cleanup

		// Store current commit hash in status
		if currentCommitHash != "" {
			cdk8sAppProxy.Status.LastRemoteGitHash = currentCommitHash
			logger.Info("Updated cdk8sAppProxy.Status.LastRemoteGitHash with the latest commit hash from remote", "lastRemoteGitHash", currentCommitHash)
		}

		// Stop any existing git poller for a local path
		if cancel, ok := r.ActiveGitPollers[proxyNamespacedName]; ok {
			logger.Info("GitRepository spec removed or empty, stopping existing git poller.")
			cancel()
			delete(r.ActiveGitPollers, proxyNamespacedName)
		}

	default:
		err := errors.New("no source specified")
		logger.Error(err, "No source specified")
		if cancel, ok := r.ActiveGitPollers[proxyNamespacedName]; ok {
			logger.Info("Source spec is invalid or removed, stopping existing git poller.")
			cancel()
			delete(r.ActiveGitPollers, proxyNamespacedName)
		}
		// Use the new consolidated error handler - removeFinalizer = false for normal operations
		_ = r.updateStatusWithError(ctx, cdk8sAppProxy, addonsv1alpha1.SourceNotSpecifiedReason, "No GitRepository specified", err, false)

		return "", "", nil, err
	}

	return appSourcePath, currentCommitHash, cleanupFunc, nil
}

func (r *Reconciler) prepareGitSource(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, logger logr.Logger) (string, string, func(), error) {
	gitSpec := cdk8sAppProxy.Spec.GitRepository
	logger.Info("Determined source type: GitRepository", "url", gitSpec.URL, "reference", gitSpec.Reference, "path", gitSpec.Path)

	tempDir, err := os.MkdirTemp("", "cdk8s-git-clone-")
	if err != nil {
		logger.Error(err, "Failed to create temp directory for git clone")
		_ = r.updateStatusWithError(ctx, cdk8sAppProxy, addonsv1alpha1.GitCloneFailedReason, "Failed to create temp dir for git clone", err, false)

		return "", "", nil, err
	}

	cleanupFunc := func() {
		if err := os.RemoveAll(tempDir); err != nil {
			logger.Error(err, "Failed to remove temporary clone directory", "tempDir", tempDir)
		}
	}

	logger.Info("Created temporary directory for clone", "tempDir", tempDir)

	// Clone repository
	if err := r.cloneGitRepository(ctx, cdk8sAppProxy, gitSpec, tempDir, logger, OperationNormal); err != nil {
		cleanupFunc()

		return "", "", nil, err
	}

	// Checkout-specific reference if specified
	if err := r.checkoutGitReference(ctx, cdk8sAppProxy, gitSpec, tempDir, logger, OperationNormal); err != nil {
		cleanupFunc()

		return "", "", nil, err
	}

	// Determine a final app source path
	appSourcePath := tempDir
	if gitSpec.Path != "" {
		appSourcePath = filepath.Join(tempDir, gitSpec.Path)
		logger.Info("Adjusted appSourcePath for repository subpath", "subPath", gitSpec.Path, "finalPath", appSourcePath)
	}

	// Get current commit hash inline
	logger.Info("Attempting to retrieve current commit hash from Git repository", "repoDir", tempDir)
	repo, err := git.PlainOpen(tempDir)
	if err != nil {
		cleanupFunc()

		return "", "", nil, r.updateStatusWithError(ctx, cdk8sAppProxy, addonsv1alpha1.GitCloneFailedReason, "Failed to open git repository: "+err.Error(), err, false)
	}

	headRef, err := repo.Head()
	if err != nil {
		cleanupFunc()

		return "", "", nil, r.updateStatusWithError(ctx, cdk8sAppProxy, addonsv1alpha1.GitCloneFailedReason, "Failed to get HEAD reference from git repository: "+err.Error(), err, false)
	}

	currentCommitHash := headRef.Hash().String()
	logger.Info("Successfully retrieved current commit hash from Git repository", "commitHash", currentCommitHash)

	return appSourcePath, currentCommitHash, cleanupFunc, nil
}

func (r *Reconciler) checkoutGitReference(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, gitSpec *addonsv1alpha1.GitRepositorySpec, tempDir string, logger logr.Logger, operation string) error {
	if gitSpec.Reference == "" {
		return nil
	}
	logger.Info("Executing git checkout with go-git", "reference", gitSpec.Reference, "dir", tempDir, "operation", operation)

	repo, err := git.PlainOpen(tempDir)
	if err != nil {
		logger.Error(err, "go-git PlainOpen failed", "operation", operation)
		if cdk8sAppProxy != nil {
			removeFinalizer := operation == OperationDeletion

			return r.updateStatusWithError(ctx, cdk8sAppProxy, addonsv1alpha1.GitCheckoutFailedReason, "go-git PlainOpen failed during "+operation, err, removeFinalizer)
		}

		return err
	}

	worktree, err := repo.Worktree()
	if err != nil {
		logger.Error(err, "go-git Worktree failed", "operation", operation)
		if cdk8sAppProxy != nil {
			removeFinalizer := operation == OperationDeletion

			return r.updateStatusWithError(ctx, cdk8sAppProxy, addonsv1alpha1.GitCheckoutFailedReason, "go-git Worktree failed during "+operation, err, removeFinalizer)
		}

		return err
	}

	checkoutOpts := &git.CheckoutOptions{Force: true}
	if plumbing.IsHash(gitSpec.Reference) {
		checkoutOpts.Hash = plumbing.NewHash(gitSpec.Reference)
	} else {
		revision := plumbing.Revision(gitSpec.Reference)
		resolvedHash, err := repo.ResolveRevision(revision)
		if err != nil {
			logger.Error(err, "go-git ResolveRevision failed", "reference", gitSpec.Reference, "operation", operation)
			if cdk8sAppProxy != nil {
				removeFinalizer := operation == OperationDeletion

				return r.updateStatusWithError(ctx, cdk8sAppProxy, addonsv1alpha1.GitCheckoutFailedReason, "go-git ResolveRevision failed for ref "+gitSpec.Reference+" during "+operation, err, removeFinalizer)
			}

			return err
		}
		checkoutOpts.Hash = *resolvedHash
	}

	err = worktree.Checkout(checkoutOpts)
	if err != nil {
		logger.Error(err, "go-git Checkout failed", "reference", gitSpec.Reference, "operation", operation)
		if cdk8sAppProxy != nil {
			removeFinalizer := operation == OperationDeletion

			return r.updateStatusWithError(ctx, cdk8sAppProxy, addonsv1alpha1.GitCheckoutFailedReason, "go-git Checkout failed for ref "+gitSpec.Reference+" during "+operation, err, removeFinalizer)
		}

		return err
	}
	logger.Info("Successfully checked out git reference with go-git", "reference", gitSpec.Reference, "operation", operation)

	return nil
}

func (gpl *gitProgressLogger) Write(p []byte) (n int, err error) {
	gpl.buffer = append(gpl.buffer, p...)
	for {
		idx := bytes.IndexByte(gpl.buffer, '\n')
		if idx == -1 {
			// If buffer gets too large without a newline, log it to prevent OOM
			if len(gpl.buffer) > 1024 {
				gpl.logger.Info(strings.TrimSpace(string(gpl.buffer)))
				gpl.buffer = nil
			}

			break
		}
		line := gpl.buffer[:idx]
		gpl.buffer = gpl.buffer[idx+1:]
		gpl.logger.Info(strings.TrimSpace(string(line)))
	}

	return len(p), nil
}
