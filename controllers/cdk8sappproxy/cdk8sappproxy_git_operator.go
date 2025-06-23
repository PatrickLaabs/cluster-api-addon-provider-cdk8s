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

// pollGitRepository periodically checks the remote git repository for changes.
func (r *Reconciler) pollGitRepository(ctx context.Context, proxyName types.NamespacedName) {
	logger := log.FromContext(ctx).WithValues("cdk8sappproxy", proxyName.String(), "goroutine", "pollGitRepository")

	cdk8sAppProxy := &addonsv1alpha1.Cdk8sAppProxy{}
	err := r.Get(ctx, proxyName, cdk8sAppProxy)
	if err != nil {
		logger.Error(err, "Failed to get cdk8sAppProxy for polling")

		return
	}

	var pollInterval time.Duration
	if cdk8sAppProxy.Spec.GitRepository.ReferencePollInterval == nil {
		pollInterval = 5 * time.Minute
	} else {
		pollInterval = cdk8sAppProxy.Spec.GitRepository.ReferencePollInterval.Duration
	}

	ticker := time.NewTicker(pollInterval)
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

	logger.Info("Attempting to LsRemote (inlined in pollGitRepositoryOnce)", "url", gitSpec.URL, "refName", refName.String())

	rem := git.NewRemote(memory.NewStorage(), &config.RemoteConfig{
		URLs: []string{gitSpec.URL},
	})

	var authGit *http.BasicAuth // Renamed from 'auth' to avoid conflict with 'err' if it was also named 'auth'
	authGit, err = r.getGitAuth(ctx, cdk8sAppProxy, gitSpec.AuthSecretRef, logger, "fetchRemoteCommitHash_inlined")
	if err != nil {
		return errors.Wrapf(err, "failed to get git auth for LsRemote in pollGitRepositoryOnce")
	}

	refs, err := rem.ListContext(ctx, &git.ListOptions{Auth: authGit})
	if err != nil {
		logger.Error(err, "Failed to LsRemote from git repository in pollGitRepositoryOnce")
		if errors.Is(err, plumbingtransport.ErrAuthenticationRequired) || strings.Contains(err.Error(), "authentication required") {
			logger.Info("Authentication failed for LsRemote in pollGitRepositoryOnce. Please check credentials.")
		}
		return errors.Wrap(err, "failed to LsRemote from git repository in pollGitRepositoryOnce")
	}

	var remoteCommitHash string
	foundRef := false
	for _, ref := range refs {
		if ref.Name() == refName {
			remoteCommitHash = ref.Hash().String()
			logger.Info("Found matching remote reference in pollGitRepositoryOnce", "refName", refName.String(), "commitHash", remoteCommitHash)
			foundRef = true
			break
		}
	}

	if !foundRef {
		err = errors.Errorf("reference '%s' not found in remote repository %s during poll", refName.String(), gitSpec.URL)
		logger.Error(err, "Specified reference not found in remote repository in pollGitRepositoryOnce", "refName", refName.String(), "url", gitSpec.URL)
		return err
	}
	
	return r.handleGitRepositoryChange(ctx, proxyName, cdk8sAppProxy, remoteCommitHash, logger)
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

func (r *Reconciler) prepareSource(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, proxyNamespacedName types.NamespacedName, logger logr.Logger, operation string) (string, string, func(), error) {
	var appSourcePath string
	var currentCommitHash string
	var cleanupFunc func()

	switch {
	case cdk8sAppProxy.Spec.GitRepository != nil && cdk8sAppProxy.Spec.GitRepository.URL != "":
		gitSpec := cdk8sAppProxy.Spec.GitRepository
		logger.Info("Determined source type: GitRepository", "url", gitSpec.URL, "reference", gitSpec.Reference, "path", gitSpec.Path, "operation", operation)

		tempDirPattern := "cdk8s-git-clone-"
		if operation == OperationDeletion {
			tempDirPattern = "cdk8s-git-delete-"
		}
		tempDir, err := os.MkdirTemp("", tempDirPattern)
		if err != nil {
			logger.Error(err, "Failed to create temp directory for git clone", "operation", operation)
			// For normal operation, update status. For deletion, the Cdk8sAppProxy might be nil or finalizer needs removal.
			if operation == OperationNormal && cdk8sAppProxy != nil {
				_ = r.updateStatusWithError(ctx, cdk8sAppProxy, addonsv1alpha1.GitCloneFailedReason, "Failed to create temp dir for git clone", err, false)
			}

			return "", "", nil, err
		}

		cleanupFunc = func() {
			if err := os.RemoveAll(tempDir); err != nil {
				logger.Error(err, "Failed to remove temporary clone directory", "tempDir", tempDir, "operation", operation)
			}
		}
		logger.Info("Created temporary directory for clone", "tempDir", tempDir, "operation", operation)

		var retrieveCommitHash string
		retrieveCommitHash, err = r.performGitOperations(ctx, cdk8sAppProxy, gitSpec, tempDir, logger, operation)
		if err != nil {
			cleanupFunc()

			return "", "", nil, errors.Wrapf(err, "failed during git operations for %s", operation)
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
		err := errors.New("no source specified (neither GitRepository nor LocalPath)")
		if operation == OperationNormal { // Poller management and status updates for normal flow
			r.stopGitPoller(proxyNamespacedName, logger)
			if cdk8sAppProxy != nil {
				_ = r.updateStatusWithError(ctx, cdk8sAppProxy, addonsv1alpha1.SourceNotSpecifiedReason, "No source specified during "+operation, err, false)
			}
		} else if operation == OperationDeletion && cdk8sAppProxy != nil {
			// For deletion, if source can't be determined, we might still want to remove finalizer.
			_ = r.updateStatusWithError(ctx, cdk8sAppProxy, addonsv1alpha1.SourceNotSpecifiedReason, "No source specified, cannot determine resources to delete during "+operation, err, true)
		}

		return "", "", nil, err
	}

	return appSourcePath, currentCommitHash, cleanupFunc, nil
}

func (r *Reconciler) performGitOperations(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, gitSpec *addonsv1alpha1.GitRepositorySpec, tempDir string, logger logr.Logger, operation string) (string, error) {
	var proxyForStatusUpdates *addonsv1alpha1.Cdk8sAppProxy
	if operation == OperationNormal {
		proxyForStatusUpdates = cdk8sAppProxy
	}

	logger.Info("Starting git clone operation", "url", gitSpec.URL, "tempDir", tempDir, "operation", operation)
	cloneOptions := &git.CloneOptions{
		URL: gitSpec.URL,
	}
	if gitSpec.AuthSecretRef != nil {
		auth, err := r.getGitAuth(ctx, proxyForStatusUpdates, gitSpec.AuthSecretRef, logger, operation) // Use proxyForStatusUpdates for auth context if needed
		if err != nil {
			// If getGitAuth fails, wrap and return error. It doesn't call updateStatusWithError itself.
			return "", errors.Wrapf(err, "failed to get git auth for clone during %s", operation)
		}
		cloneOptions.Auth = auth
	}
	logger.Info("Executing git clone with go-git (inlined)", "url", gitSpec.URL, "targetDir", tempDir, "operation", operation)
	_, err := git.PlainCloneContext(ctx, tempDir, false, cloneOptions)
	if err != nil {
		reason := addonsv1alpha1.GitCloneFailedReason
		if errors.Is(err, plumbingtransport.ErrAuthenticationRequired) || strings.Contains(err.Error(), "authentication required") {
			reason = addonsv1alpha1.GitAuthenticationFailedReason
		}
		if proxyForStatusUpdates != nil {
			removeFinalizer := operation == OperationDeletion // This logic might need refinement if OperationDeletion is not expected here
			// It's better if updateStatusWithError is called with the original cdk8sAppProxy from prepareSource if operation is Normal
			callingProxy := cdk8sAppProxy // Use the main proxy for status updates if available
			if operation != OperationNormal {
				callingProxy = proxyForStatusUpdates // Which would be nil for deletion if cdk8sAppProxy in prepareSource was nil
			}
			if callingProxy != nil {
				return "", r.updateStatusWithError(ctx, callingProxy, reason, "go-git PlainCloneContext failed during "+operation, err, removeFinalizer)
			}
		}
		return "", errors.Wrapf(err, "go-git PlainCloneContext failed during %s (no status update)", operation) // Error if no proxy for status
	}
	logger.Info("Git clone operation completed successfully", "operation", operation)

	if gitSpec.Reference != "" {
		logger.Info("Starting git checkout operation", "reference", gitSpec.Reference, "dir", tempDir, "operation", operation)
		repo, err := git.PlainOpen(tempDir)
		if err != nil {
			callingProxy := cdk8sAppProxy
			if operation != OperationNormal {
				callingProxy = proxyForStatusUpdates
			}
			if callingProxy != nil {
				return "", r.updateStatusWithError(ctx, callingProxy, addonsv1alpha1.GitCheckoutFailedReason, "go-git PlainOpen failed during "+operation+" for checkout", err, operation == OperationDeletion)
			}
			return "", errors.Wrapf(err, "go-git PlainOpen failed during %s for checkout (no status update)", operation)
		}

		worktree, err := repo.Worktree()
		if err != nil {
			callingProxy := cdk8sAppProxy
			if operation != OperationNormal {
				callingProxy = proxyForStatusUpdates
			}
			if callingProxy != nil {
				return "", r.updateStatusWithError(ctx, callingProxy, addonsv1alpha1.GitCheckoutFailedReason, "go-git Worktree failed during "+operation+" for checkout", err, operation == OperationDeletion)
			}
			return "", errors.Wrapf(err, "go-git Worktree failed during %s for checkout (no status update)", operation)
		}

		checkoutOpts := &git.CheckoutOptions{Force: true}
		if plumbing.IsHash(gitSpec.Reference) {
			checkoutOpts.Hash = plumbing.NewHash(gitSpec.Reference)
		} else {
			revision := plumbing.Revision(gitSpec.Reference)
			resolvedHash, err := repo.ResolveRevision(revision)
			if err != nil {
				callingProxy := cdk8sAppProxy
				if operation != OperationNormal {
					callingProxy = proxyForStatusUpdates
				}
				if callingProxy != nil {
					return "", r.updateStatusWithError(ctx, callingProxy, addonsv1alpha1.GitCheckoutFailedReason, "go-git ResolveRevision failed for ref "+gitSpec.Reference+" during "+operation, err, operation == OperationDeletion)
				}
				return "", errors.Wrapf(err, "go-git ResolveRevision failed for ref %s during %s (no status update)", gitSpec.Reference, operation)
			}
			checkoutOpts.Hash = *resolvedHash
		}

		err = worktree.Checkout(checkoutOpts)
		if err != nil {
			callingProxy := cdk8sAppProxy
			if operation != OperationNormal {
				callingProxy = proxyForStatusUpdates
			}
			if callingProxy != nil {
				return "", r.updateStatusWithError(ctx, callingProxy, addonsv1alpha1.GitCheckoutFailedReason, "go-git Checkout failed for ref "+gitSpec.Reference+" during "+operation, err, operation == OperationDeletion)
			}
			return "", errors.Wrapf(err, "go-git Checkout failed for ref %s during %s (no status update)", gitSpec.Reference, operation)
		}
		logger.Info("Git checkout operation completed successfully", "reference", gitSpec.Reference, "operation", operation)
	} else {
		logger.Info("No specific git reference to checkout, using default branch", "operation", operation)
	}

	var currentCommitHash string
	if operation == OperationNormal {
		logger.Info("Attempting to retrieve current commit hash from Git repository", "tempDir", tempDir, "operation", operation)
		repo, err := git.PlainOpen(tempDir)
		if err != nil {
			// For OperationNormal, cdk8sAppProxy should be non-nil.
			if cdk8sAppProxy != nil {
				err := r.updateStatusWithError(ctx, cdk8sAppProxy, addonsv1alpha1.GitOperationFailedReason,
					"Failed to open git repository post-clone/checkout after "+operation, err, false)
				if err != nil {
					return "", err
				}
			}
			return "", errors.Wrapf(err, "failed to open git repository at %s after %s", tempDir, operation)
		}

		headRef, err := repo.Head()
		if err != nil {
			if cdk8sAppProxy != nil {
				err := r.updateStatusWithError(ctx, cdk8sAppProxy, addonsv1alpha1.GitOperationFailedReason,
					"Failed to get HEAD reference from git repository post-clone/checkout after "+operation, err, false)
				if err != nil {
					return "", err
				}
			}
			return "", errors.Wrapf(err, "failed to get HEAD reference for repository at %s after %s", tempDir, operation)
		}
		currentCommitHash = headRef.Hash().String()
		logger.Info("Successfully retrieved current commit hash from Git repository", "commitHash", currentCommitHash, "operation", operation)
	}

	return currentCommitHash, nil
}

func (r *Reconciler) checkoutGitReference(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, gitSpec *addonsv1alpha1.GitRepositorySpec, tempDir string, logger logr.Logger, operation string) error {
	logger.Info("Executing git checkout with go-git", "reference", gitSpec.Reference, "dir", tempDir, "operation", operation)

	repo, err := git.PlainOpen(tempDir)
	if err != nil {
		if cdk8sAppProxy != nil {
			removeFinalizer := operation == OperationDeletion

			return r.updateStatusWithError(ctx, cdk8sAppProxy, addonsv1alpha1.GitCheckoutFailedReason, "go-git PlainOpen failed during "+operation, err, removeFinalizer)
		}

		return err
	}

	worktree, err := repo.Worktree()
	if err != nil {
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
		if cdk8sAppProxy != nil {
			removeFinalizer := operation == OperationDeletion

			return r.updateStatusWithError(ctx, cdk8sAppProxy, addonsv1alpha1.GitCheckoutFailedReason, "go-git Checkout failed for ref "+gitSpec.Reference+" during "+operation, err, removeFinalizer)
		}

		return err
	}
	logger.Info("Successfully checked out git reference with go-git", "reference", gitSpec.Reference, "operation", operation)

	return nil
}
