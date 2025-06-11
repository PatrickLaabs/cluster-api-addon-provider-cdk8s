package cdk8sappproxy

import (
	"context"
	"os"
	"path/filepath"

	addonsv1alpha1 "github.com/PatrickLaabs/cluster-api-addon-provider-cdk8s/api/v1alpha1"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

//+kubebuilder:rbac:groups=addons.cluster.x-k8s.io,resources=cdk8sappproxies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=addons.cluster.x-k8s.io,resources=cdk8sappproxies/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=addons.cluster.x-k8s.io,resources=cdk8sappproxies/finalizers,verbs=update
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;watch
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters/status,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("cdk8sappproxy", req.NamespacedName)
	logger.Info("Starting Reconcile")

	// Initialize activeGitPollers map if it's nil
	if r.activeGitPollers == nil {
		r.activeGitPollers = make(map[types.NamespacedName]context.CancelFunc)
	}
	// Initialize ActiveWatches map if it's nil
	// Note: This was moved from reconcileNormal to ensure it's initialized before any delete or normal path.
	if r.ActiveWatches == nil {
		r.ActiveWatches = make(map[types.NamespacedName]map[string]context.CancelFunc)
	}

	cdk8sAppProxy := &addonsv1alpha1.Cdk8sAppProxy{}
	if err := r.Get(ctx, req.NamespacedName, cdk8sAppProxy); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("Cdk8sAppProxy resource not found. Ignoring since object must be deleted.")

			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get Cdk8sAppProxy")

		return ctrl.Result{}, errors.Wrapf(err, "failed to get Cdk8sAppProxy %s/%s", req.Namespace, req.Name)
	}

	logger = logger.WithValues("name", cdk8sAppProxy.Name, "namespace", cdk8sAppProxy.Namespace)
	logger.Info("Fetched Cdk8sAppProxy", "deletionTimestamp", cdk8sAppProxy.DeletionTimestamp)

	if !cdk8sAppProxy.DeletionTimestamp.IsZero() {
		logger.Info("Cdk8sAppProxy is being deleted, reconciling delete.")

		return r.reconcileDelete(ctx, cdk8sAppProxy)
	}
	logger.Info("Cdk8sAppProxy is not being deleted, reconciling normal.")

	return r.reconcileNormal(ctx, cdk8sAppProxy)
}

func (r *Reconciler) reconcileDelete(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("cdk8sappproxy", types.NamespacedName{Name: cdk8sAppProxy.Name, Namespace: cdk8sAppProxy.Namespace}, "reconcile_type", "delete")
	logger.Info("Starting reconcileDelete")

	proxyNamespacedName := types.NamespacedName{Name: cdk8sAppProxy.Name, Namespace: cdk8sAppProxy.Namespace}

	// Stop any active git poller
	r.stopGitPoller(proxyNamespacedName, logger)

	if !controllerutil.ContainsFinalizer(cdk8sAppProxy, Finalizer) {
		logger.Info("Finalizer already removed, nothing to do.")

		return ctrl.Result{}, nil
	}

	// Get a source path for deletion
	appSourcePath, cleanup, err := r.prepareSourceForDeletion(ctx, cdk8sAppProxy, logger)
	if err != nil {
		return ctrl.Result{}, err
	}
	defer cleanup()

	// Get resources to delete
	parsedResources, err := r.synthesizeAndParseResources(appSourcePath, logger)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Delete resources from target clusters
	if err := r.deleteResourcesFromClusters(ctx, cdk8sAppProxy, parsedResources, logger); err != nil {
		return ctrl.Result{}, err
	}

	// Clean up watches and remove finalizer
	return r.finalizeDeletion(ctx, cdk8sAppProxy, proxyNamespacedName, logger)
}

func (r *Reconciler) reconcileNormal(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("cdk8sappproxy", types.NamespacedName{Name: cdk8sAppProxy.Name, Namespace: cdk8sAppProxy.Namespace}, "reconcile_type", "normal")
	logger.Info("Starting reconcileNormal")

	proxyNamespacedName := types.NamespacedName{Name: cdk8sAppProxy.Name, Namespace: cdk8sAppProxy.Namespace}

	// Handle deletion trigger annotation
	forceSynthAndApplyDueToDeletion, err := r.handleDeletionTriggerAnnotation(ctx, cdk8sAppProxy, logger)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Initialize active watches for this proxy
	r.initializeActiveWatches(proxyNamespacedName)

	// Add finalizer if needed
	if shouldRequeue, err := r.ensureFinalizer(ctx, cdk8sAppProxy, logger); err != nil || shouldRequeue {
		return ctrl.Result{Requeue: shouldRequeue}, err
	}

	// Prepare a source path and get current commit hash
	appSourcePath, currentCommitHash, cleanup, err := r.prepareSource(ctx, cdk8sAppProxy, proxyNamespacedName, logger)
	if err != nil {
		return ctrl.Result{}, err
	}
	defer cleanup()

	// Manage git poller lifecycle
	r.manageGitPollerLifecycle(ctx, cdk8sAppProxy, proxyNamespacedName, logger)

	// Synthesize and parse resources
	parsedResources, err := r.synthesizeAndParseResources(appSourcePath, logger)
	if err != nil {
		return ctrl.Result{}, err
	}

	if len(parsedResources) == 0 {
		if err := r.handleNoResources(ctx, cdk8sAppProxy, logger); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	// Determine if apply is needed
	applyNeeded, clusterList, err := r.determineIfApplyNeeded(ctx, cdk8sAppProxy, parsedResources, currentCommitHash, forceSynthAndApplyDueToDeletion, logger)
	if err != nil {
		return ctrl.Result{}, err
	}

	if !applyNeeded {
		if err := r.handleSkipApply(ctx, cdk8sAppProxy, currentCommitHash, logger); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	// Apply resources to clusters
	return r.applyResourcesToClusters(ctx, cdk8sAppProxy, parsedResources, clusterList, currentCommitHash, proxyNamespacedName, logger)
}

func (r *Reconciler) handleDeletionTriggerAnnotation(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, logger logr.Logger) (bool, error) {
	deletionTriggerAnnotationKey := "cdk8s.addons.cluster.x-k8s.io/reconcile-on-delete-trigger"
	forceSynthAndApplyDueToDeletion := false

	if cdk8sAppProxy.Annotations != nil {
		if _, ok := cdk8sAppProxy.Annotations[deletionTriggerAnnotationKey]; ok {
			forceSynthAndApplyDueToDeletion = true
			logger.Info("Reconciliation was triggered by a resource deletion annotation.", "annotationKey", deletionTriggerAnnotationKey)

			// Clear the annotation
			logger.Info("Attempting to clear the resource deletion trigger annotation.", "annotationKey", deletionTriggerAnnotationKey)
			delete(cdk8sAppProxy.Annotations, deletionTriggerAnnotationKey)
			if len(cdk8sAppProxy.Annotations) == 0 {
				cdk8sAppProxy.Annotations = nil
			}

			if err := r.Update(ctx, cdk8sAppProxy); err != nil {
				logger.Error(err, "Failed to clear the resource deletion trigger annotation. Requeuing.", "annotationKey", deletionTriggerAnnotationKey)

				return false, err
			}
			logger.Info("Successfully cleared the resource deletion trigger annotation.", "annotationKey", deletionTriggerAnnotationKey)
		}
	}

	return forceSynthAndApplyDueToDeletion, nil
}

func (r *Reconciler) initializeActiveWatches(proxyNamespacedName types.NamespacedName) {
	if r.ActiveWatches[proxyNamespacedName] == nil {
		r.ActiveWatches[proxyNamespacedName] = make(map[string]context.CancelFunc)
	}
}

func (r *Reconciler) ensureFinalizer(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, logger logr.Logger) (bool, error) {
	if !controllerutil.ContainsFinalizer(cdk8sAppProxy, Finalizer) {
		logger.Info("Adding finalizer", "finalizer", Finalizer)
		controllerutil.AddFinalizer(cdk8sAppProxy, Finalizer)
		if err := r.Update(ctx, cdk8sAppProxy); err != nil {
			logger.Error(err, "Failed to add finalizer")

			return false, err
		}
		logger.Info("Successfully added finalizer")

		return true, nil
	}

	return false, nil
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

	case cdk8sAppProxy.Spec.LocalPath != "":
		logger.Info("Determined source type: LocalPath", "path", cdk8sAppProxy.Spec.LocalPath)
		appSourcePath = cdk8sAppProxy.Spec.LocalPath
		cleanupFunc = func() {} // No cleanup needed for a local path

		// Stop any existing git poller for a local path
		if cancel, ok := r.activeGitPollers[proxyNamespacedName]; ok {
			logger.Info("GitRepository spec removed or empty, stopping existing git poller.")
			cancel()
			delete(r.activeGitPollers, proxyNamespacedName)
		}

	default:
		err := errors.New("no source specified")
		logger.Error(err, "No source specified (neither GitRepository nor LocalPath)")
		if cancel, ok := r.activeGitPollers[proxyNamespacedName]; ok {
			logger.Info("Source spec is invalid or removed, stopping existing git poller.")
			cancel()
			delete(r.activeGitPollers, proxyNamespacedName)
		}
		_ = r.handleError(ctx, cdk8sAppProxy, addonsv1alpha1.SourceNotSpecifiedReason, "Neither GitRepository nor LocalPath specified", err)

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
		_ = r.handleError(ctx, cdk8sAppProxy, addonsv1alpha1.GitCloneFailedReason, "Failed to create temp dir for git clone", err)

		return "", "", nil, err
	}

	cleanupFunc := func() {
		logger.Info("Removing temporary clone directory", "tempDir", tempDir)
		if err := os.RemoveAll(tempDir); err != nil {
			logger.Error(err, "Failed to remove temporary clone directory", "tempDir", tempDir)
		}
	}

	logger.Info("Created temporary directory for clone", "tempDir", tempDir)

	// Clone repository
	if err := r.cloneGitRepository(ctx, cdk8sAppProxy, gitSpec, tempDir, logger, "prepareGitSource"); err != nil {
		cleanupFunc()

		return "", "", nil, err
	}

	// Checkout-specific reference if specified
	if err := r.checkoutReference(ctx, cdk8sAppProxy, gitSpec, tempDir, logger); err != nil {
		cleanupFunc()

		return "", "", nil, err
	}

	// Determine a final app source path
	appSourcePath := tempDir
	if gitSpec.Path != "" {
		appSourcePath = filepath.Join(tempDir, gitSpec.Path)
		logger.Info("Adjusted appSourcePath for repository subpath", "subPath", gitSpec.Path, "finalPath", appSourcePath)
	}

	// Get current commit hash
	currentCommitHash, err := r.getCurrentCommitHash(tempDir, logger)
	if err != nil {
		cleanupFunc()

		return "", "", nil, r.handleError(ctx, cdk8sAppProxy, addonsv1alpha1.GitCloneFailedReason, "Failed to get commit hash: "+err.Error(), err)
	}

	return appSourcePath, currentCommitHash, cleanupFunc, nil
}

func (r *Reconciler) checkoutReference(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, gitSpec *addonsv1alpha1.GitRepositorySpec, tempDir string, logger logr.Logger) error {
	if gitSpec.Reference == "" {
		return nil
	}

	logger.Info("Executing git checkout with go-git", "reference", gitSpec.Reference, "dir", tempDir)
	repo, err := git.PlainOpen(tempDir)
	if err != nil {
		logger.Error(err, "go-git PlainOpen failed")

		return r.handleError(ctx, cdk8sAppProxy, addonsv1alpha1.GitCheckoutFailedReason, "go-git PlainOpen failed", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		logger.Error(err, "go-git Worktree failed")

		return r.handleError(ctx, cdk8sAppProxy, addonsv1alpha1.GitCheckoutFailedReason, "go-git Worktree failed", err)
	}

	checkoutOpts := &git.CheckoutOptions{Force: true}
	if plumbing.IsHash(gitSpec.Reference) {
		checkoutOpts.Hash = plumbing.NewHash(gitSpec.Reference)
	} else {
		revision := plumbing.Revision(gitSpec.Reference)
		resolvedHash, err := repo.ResolveRevision(revision)
		if err != nil {
			logger.Error(err, "go-git ResolveRevision failed", "reference", gitSpec.Reference)

			return r.handleError(ctx, cdk8sAppProxy, addonsv1alpha1.GitCheckoutFailedReason, "go-git ResolveRevision failed for ref "+gitSpec.Reference, err)
		}
		checkoutOpts.Hash = *resolvedHash
	}

	err = worktree.Checkout(checkoutOpts)
	if err != nil {
		logger.Error(err, "go-git Checkout failed", "reference", gitSpec.Reference)

		return r.handleError(ctx, cdk8sAppProxy, addonsv1alpha1.GitCheckoutFailedReason, "go-git Checkout failed for ref "+gitSpec.Reference, err)
	}

	logger.Info("Successfully checked out git reference with go-git", "reference", gitSpec.Reference)

	return nil
}

func (r *Reconciler) getCurrentCommitHash(tempDir string, logger logr.Logger) (string, error) {
	logger.Info("Attempting to retrieve current commit hash from Git repository", "repoDir", tempDir)
	repo, err := git.PlainOpen(tempDir)
	if err != nil {
		logger.Error(err, "Failed to open git repository after clone/checkout")

		return "", err
	}

	headRef, err := repo.Head()
	if err != nil {
		logger.Error(err, "Failed to get HEAD reference from git repository")

		return "", err
	}

	commitHash := headRef.Hash().String()
	logger.Info("Successfully retrieved current commit hash from Git repository", "commitHash", commitHash)

	return commitHash, nil
}

func (r *Reconciler) manageGitPollerLifecycle(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, proxyNamespacedName types.NamespacedName, logger logr.Logger) {
	if cdk8sAppProxy.Spec.GitRepository != nil && cdk8sAppProxy.Spec.GitRepository.URL != "" {
		if _, pollerExists := r.activeGitPollers[proxyNamespacedName]; !pollerExists {
			logger.Info("Starting new git poller.")
			pollCtx, cancelPoll := context.WithCancel(ctx)
			r.activeGitPollers[proxyNamespacedName] = cancelPoll
			go r.pollGitRepository(pollCtx, proxyNamespacedName)
		} else {
			logger.Info("Git poller already active.")
		}
	} else {
		if cancel, pollerExists := r.activeGitPollers[proxyNamespacedName]; pollerExists {
			logger.Info("GitRepository is not configured, ensuring poller is stopped.")
			cancel()
			delete(r.activeGitPollers, proxyNamespacedName)
		}
	}
}

func (r *Reconciler) handleNoResources(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, logger logr.Logger) error {
	logger.Info("No valid Kubernetes resources parsed from manifest files")
	conditions.MarkFalse(cdk8sAppProxy, addonsv1alpha1.DeploymentProgressingCondition, addonsv1alpha1.NoResourcesParsedReason, clusterv1.ConditionSeverityWarning, "No valid Kubernetes resources found in manifests")
	if err := r.Status().Update(ctx, cdk8sAppProxy); err != nil {
		logger.Error(err, "Failed to update status after no resources parsed")

		return err
	}

	return nil
}

func (r *Reconciler) determineIfApplyNeeded(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, parsedResources []*unstructured.Unstructured, currentCommitHash string, forceSynthAndApplyDueToDeletion bool, logger logr.Logger) (bool, clusterv1.ClusterList, error) {
	var clusterList clusterv1.ClusterList

	// Check for git or annotation triggers
	triggeredByGitOrAnnotation := r.checkGitOrAnnotationTriggers(cdk8sAppProxy, currentCommitHash, forceSynthAndApplyDueToDeletion, logger)

	if !triggeredByGitOrAnnotation {
		// Check if resources are missing on clusters
		foundMissingResources, list, err := r.verifyResourcesOnClusters(ctx, cdk8sAppProxy, parsedResources, logger)
		if err != nil {
			return false, clusterList, err
		}
		clusterList = list

		return foundMissingResources, clusterList, nil
	}

	return true, clusterList, nil
}

func (r *Reconciler) verifyResourcesOnClusters(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, parsedResources []*unstructured.Unstructured, logger logr.Logger) (bool, clusterv1.ClusterList, error) {
	var clusterList clusterv1.ClusterList
	foundMissingResourcesOnAnyCluster := false

	if len(parsedResources) == 0 {
		logger.Info("No parsed resources to verify. Skipping resource verification.")

		return false, clusterList, nil
	}

	selector, err := metav1.LabelSelectorAsSelector(&cdk8sAppProxy.Spec.ClusterSelector)
	if err != nil {
		logger.Error(err, "Failed to parse ClusterSelector for verification, assuming resources might be missing.", "selector", cdk8sAppProxy.Spec.ClusterSelector)

		return true, clusterList, r.handleError(ctx, cdk8sAppProxy, addonsv1alpha1.ClusterSelectorParseFailedReason, "Failed to parse ClusterSelector for verification", err)
	}

	logger.Info("Listing clusters for resource verification", "selector", selector.String(), "namespace", cdk8sAppProxy.Namespace)
	if err := r.List(ctx, &clusterList, client.MatchingLabelsSelector{Selector: selector}); err != nil {
		logger.Error(err, "Failed to list clusters for verification, assuming resources might be missing.")

		return true, clusterList, r.handleError(ctx, cdk8sAppProxy, addonsv1alpha1.ListClustersFailedReason, "Failed to list clusters for verification", err)
	}

	if len(clusterList.Items) == 0 {
		logger.Info("No clusters found matching selector for verification. Skipping resource verification on clusters.")

		return false, clusterList, nil
	}

	logger.Info("Successfully listed clusters for verification", "count", len(clusterList.Items))
	for _, cluster := range clusterList.Items {
		clusterLogger := logger.WithValues("targetCluster", cluster.Name)
		clusterLogger.Info("Verifying resources on cluster")
		dynamicClient, err := r.getDynamicClientForCluster(ctx, cluster.Namespace, cluster.Name)
		if err != nil {
			clusterLogger.Error(err, "Failed to get dynamic client for verification. Assuming resources missing on this cluster.")
			foundMissingResourcesOnAnyCluster = true

			break
		}
		for _, resource := range parsedResources {
			gvr := resource.GroupVersionKind().GroupVersion().WithResource(getPluralFromKind(resource.GetKind()))
			exists, checkErr := checkIfResourceExists(ctx, dynamicClient, gvr, resource.GetNamespace(), resource.GetName())
			if checkErr != nil {
				clusterLogger.Error(checkErr, "Error checking resource existence. Assuming resource missing.", "resourceName", resource.GetName(), "GVK", gvr)
				foundMissingResourcesOnAnyCluster = true

				break
			}
			if !exists {
				clusterLogger.Info("Resource missing on target cluster.", "resourceName", resource.GetName(), "GVK", gvr, "namespace", resource.GetNamespace())
				foundMissingResourcesOnAnyCluster = true

				break
			}
		}
		if foundMissingResourcesOnAnyCluster {
			break
		}
	}

	return foundMissingResourcesOnAnyCluster, clusterList, nil
}

func (r *Reconciler) checkGitOrAnnotationTriggers(cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, currentCommitHash string, forceSynthAndApplyDueToDeletion bool, logger logr.Logger) bool {
	// Check for periodic git poller trigger
	if cdk8sAppProxy.Status.LastRemoteGitHash != "" &&
		cdk8sAppProxy.Status.LastRemoteGitHash != cdk8sAppProxy.Status.LastProcessedGitHash &&
		cdk8sAppProxy.Status.LastRemoteGitHash != currentCommitHash {
		logger.Info("Reconciliation proceeding due to change detected by git poller.",
			"lastRemoteGitHash", cdk8sAppProxy.Status.LastRemoteGitHash,
			"lastProcessedGitHash", cdk8sAppProxy.Status.LastProcessedGitHash,
			"currentCommitHash", currentCommitHash)

		return true
	}

	// Check for git repository changes
	if cdk8sAppProxy.Spec.GitRepository != nil && cdk8sAppProxy.Spec.GitRepository.URL != "" {
		if currentCommitHash == "" {
			logger.Info("currentCommitHash is unexpectedly empty for Git source; proceeding with update as a precaution.")

			return true
		}

		lastProcessedGitHash := cdk8sAppProxy.Status.LastProcessedGitHash
		gitSpecRef := cdk8sAppProxy.Spec.GitRepository.Reference
		repositoryHasChanged := currentCommitHash != lastProcessedGitHash
		isInitialDeployment := lastProcessedGitHash == ""

		if isInitialDeployment {
			logger.Info("Initial deployment or no last processed hash found. Proceeding with cdk8s synth and apply.", "currentCommitHash", currentCommitHash, "reference", gitSpecRef)

			return true
		}
		if repositoryHasChanged {
			logger.Info("Git repository has changed (current clone vs last processed), proceeding with update.", "currentCommitHash", currentCommitHash, "lastProcessedGitHash", lastProcessedGitHash, "reference", gitSpecRef)

			return true
		}
		logger.Info("No new Git changes detected (current clone matches last processed, and no pending poller detection).", "commitHash", currentCommitHash, "reference", gitSpecRef)
	} else if cdk8sAppProxy.Spec.LocalPath != "" && cdk8sAppProxy.Status.ObservedGeneration == 0 {
		logger.Info("Initial processing for LocalPath or source type without explicit change detection. Proceeding with cdk8s synth and apply.")

		return true
	}

	// Check for deletion trigger
	if forceSynthAndApplyDueToDeletion {
		logger.Info("Forcing synth and apply because reconciliation was triggered by a resource deletion")

		return true
	}

	return false
}

func (r *Reconciler) handleSkipApply(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, currentCommitHash string, logger logr.Logger) error {
	logger.Info("Skipping resource application: no Git changes, no deletion annotation, and all resources verified present.")
	cdk8sAppProxy.Status.ObservedGeneration = cdk8sAppProxy.Generation
	conditions.MarkTrue(cdk8sAppProxy, addonsv1alpha1.DeploymentProgressingCondition)

	if cdk8sAppProxy.Spec.GitRepository != nil && cdk8sAppProxy.Spec.GitRepository.URL != "" && currentCommitHash != "" {
		cdk8sAppProxy.Status.LastProcessedGitHash = currentCommitHash
		logger.Info("Updated LastProcessedGitHash to current commit hash as no changes or missing resources were found.", "hash", currentCommitHash)
	}

	if err := r.Status().Update(ctx, cdk8sAppProxy); err != nil {
		logger.Error(err, "Failed to update status after skipping resource application.")

		return err
	}

	return nil
}

//nolint:unparam // ctrl.Result is required for controller-runtime reconciler pattern
func (r *Reconciler) applyResourcesToClusters(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, parsedResources []*unstructured.Unstructured, clusterList clusterv1.ClusterList, currentCommitHash string, proxyNamespacedName types.NamespacedName, logger logr.Logger) (ctrl.Result, error) {
	logger.Info("Proceeding with application of resources to target clusters.")

	// Ensure clusterList is populated if needed
	if len(clusterList.Items) == 0 && len(parsedResources) > 0 {
		logger.Info("Cluster list for apply phase is empty, re-listing.")
		selector, err := metav1.LabelSelectorAsSelector(&cdk8sAppProxy.Spec.ClusterSelector)
		if err != nil {
			logger.Error(err, "Failed to parse ClusterSelector for application phase", "selector", cdk8sAppProxy.Spec.ClusterSelector)

			return ctrl.Result{}, r.handleError(ctx, cdk8sAppProxy, addonsv1alpha1.ClusterSelectorParseFailedReason, "Failed to parse ClusterSelector for application", err)
		}
		logger.Info("Listing clusters for application phase", "selector", selector.String(), "namespace", cdk8sAppProxy.Namespace)
		if err := r.List(ctx, &clusterList, client.MatchingLabelsSelector{Selector: selector}); err != nil {
			logger.Error(err, "Failed to list clusters for application phase")

			return ctrl.Result{}, r.handleError(ctx, cdk8sAppProxy, addonsv1alpha1.ListClustersFailedReason, "Failed to list clusters for application", err)
		}
		if len(clusterList.Items) == 0 {
			logger.Info("No clusters found matching the selector for application phase.")
			conditions.MarkFalse(cdk8sAppProxy, addonsv1alpha1.DeploymentProgressingCondition, addonsv1alpha1.NoMatchingClustersReason, clusterv1.ConditionSeverityInfo, "No clusters found matching selector for application")
			if errStatusUpdate := r.Status().Update(ctx, cdk8sAppProxy); errStatusUpdate != nil {
				logger.Error(errStatusUpdate, "Failed to update status when no matching clusters found for application")
			}

			return ctrl.Result{}, nil
		}
		logger.Info("Successfully listed clusters for application phase", "count", len(clusterList.Items))
	} else if len(parsedResources) == 0 {
		logger.Info("No parsed resources to apply, skipping application to clusters.")
		cdk8sAppProxy.Status.ObservedGeneration = cdk8sAppProxy.Generation
		conditions.MarkTrue(cdk8sAppProxy, addonsv1alpha1.DeploymentProgressingCondition)
		if cdk8sAppProxy.Spec.GitRepository != nil && cdk8sAppProxy.Spec.GitRepository.URL != "" && currentCommitHash != "" {
			cdk8sAppProxy.Status.LastProcessedGitHash = currentCommitHash
		}
		if err := r.Status().Update(ctx, cdk8sAppProxy); err != nil {
			logger.Error(err, "Failed to update status when no resources to apply")

			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	var overallSuccess = true
	var firstErrorEncountered error

	for _, cluster := range clusterList.Items {
		clusterLogger := logger.WithValues("targetCluster", cluster.Name)
		clusterLogger.Info("Processing cluster for resource application")
		dynamicClient, err := r.getDynamicClientForCluster(ctx, cluster.Namespace, cluster.Name)
		if err != nil {
			clusterLogger.Error(err, "Failed to get dynamic client for cluster application")
			overallSuccess = false
			if firstErrorEncountered == nil {
				firstErrorEncountered = err
			}
			conditions.MarkFalse(cdk8sAppProxy, addonsv1alpha1.DeploymentProgressingCondition, addonsv1alpha1.KubeconfigUnavailableReason, clusterv1.ConditionSeverityError, "Failed to get dynamic client for cluster %s: %v", cluster.Name, err)

			continue
		}
		clusterLogger.Info("Successfully created dynamic client for cluster application")
		for _, resource := range parsedResources {
			resourceCopy := resource.DeepCopy()
			gvk := resourceCopy.GroupVersionKind()
			gvr := gvk.GroupVersion().WithResource(getPluralFromKind(gvk.Kind))
			applyOpts := metav1.ApplyOptions{FieldManager: "cdk8sappproxy-controller", Force: true}

			clusterLogger.Info("Applying resource", "GVK", gvk.String(), "Name", resourceCopy.GetName(), "Namespace", resourceCopy.GetNamespace())
			appliedResource, applyErr := dynamicClient.Resource(gvr).Namespace(resourceCopy.GetNamespace()).Apply(ctx, resourceCopy.GetName(), resourceCopy, applyOpts)
			if applyErr != nil {
				clusterLogger.Error(applyErr, "Failed to apply resource to cluster", "resourceName", resourceCopy.GetName())
				overallSuccess = false
				if firstErrorEncountered == nil {
					firstErrorEncountered = applyErr
				}
				conditions.MarkFalse(cdk8sAppProxy, addonsv1alpha1.DeploymentProgressingCondition, addonsv1alpha1.ResourceApplyFailedReason, clusterv1.ConditionSeverityError, "Failed to apply %s %s to cluster %s: %v", gvk.Kind, resourceCopy.GetName(), cluster.Name, applyErr)
			} else {
				clusterLogger.Info("Successfully applied resource to cluster", "resourceName", resourceCopy.GetName())
				watchKey := string(cluster.GetUID()) + "/" + resourceCopy.GetNamespace() + "/" + resourceCopy.GetName() + "/" + gvk.String()
				if cancel, ok := r.ActiveWatches[proxyNamespacedName][watchKey]; ok {
					clusterLogger.Info("Cancelling existing watch for resource", "watchKey", watchKey)
					cancel()
					delete(r.ActiveWatches[proxyNamespacedName], watchKey)
				}
				watchCtx, cancelWatch := context.WithCancel(ctx)
				r.ActiveWatches[proxyNamespacedName][watchKey] = cancelWatch
				go r.watchResourceOnTargetCluster(
					watchCtx,
					dynamicClient,
					gvk,
					appliedResource.GetNamespace(),
					appliedResource.GetName(),
					proxyNamespacedName,
					watchKey,
				)
			}
		}
	}

	if !overallSuccess {
		logger.Error(firstErrorEncountered, "One or more errors occurred during resource application to clusters")

		return ctrl.Result{}, firstErrorEncountered
	}

	// If we reach here, the overallSuccess is true.
	if cdk8sAppProxy.Spec.GitRepository != nil && cdk8sAppProxy.Spec.GitRepository.URL != "" {
		cdk8sAppProxy.Status.LastProcessedGitHash = currentCommitHash
		logger.Info("Successfully updated LastProcessedGitHash in status after application", "hash", currentCommitHash)
	}

	cdk8sAppProxy.Status.ObservedGeneration = cdk8sAppProxy.Generation
	conditions.MarkTrue(cdk8sAppProxy, addonsv1alpha1.DeploymentProgressingCondition)
	if err := r.Status().Update(ctx, cdk8sAppProxy); err != nil {
		logger.Error(err, "Failed to update status after successful reconciliation and application")

		return ctrl.Result{}, err
	}
	logger.Info("Successfully reconciled Cdk8sAppProxy and applied/verified resources.")

	return ctrl.Result{}, nil
}
