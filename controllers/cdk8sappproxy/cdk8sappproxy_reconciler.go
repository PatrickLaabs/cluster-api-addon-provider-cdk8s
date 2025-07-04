package cdk8sappproxy

import (
	"bytes"
	"context"
	gitoperator "github.com/PatrickLaabs/cluster-api-addon-provider-cdk8s/controllers/cdk8sappproxy/git"
	"time"

	addonsv1alpha1 "github.com/PatrickLaabs/cluster-api-addon-provider-cdk8s/api/v1alpha1"
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

	proxyNamespacedName := types.NamespacedName{Name: cdk8sAppProxy.Name, Namespace: cdk8sAppProxy.Namespace}

	if !controllerutil.ContainsFinalizer(cdk8sAppProxy, Finalizer) {
		logger.Info("Finalizer already removed, nothing to do.")

		return ctrl.Result{}, nil
	}

	// Clean up watches and remove finalizer
	if err := r.finalizeDeletion(ctx, cdk8sAppProxy, proxyNamespacedName, logger); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *Reconciler) reconcileNormal(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("cdk8sappproxy", types.NamespacedName{Name: cdk8sAppProxy.Name, Namespace: cdk8sAppProxy.Namespace}, "reconcile_type", "normal")
	logger.Info("Starting reconcileNormal")
	gitImpl := &gitoperator.GitImplementer{}

	proxyNamespacedName := types.NamespacedName{Name: cdk8sAppProxy.Name, Namespace: cdk8sAppProxy.Namespace}

	// Add finalizer if needed
	if shouldRequeue, err := r.ensureFinalizer(ctx, cdk8sAppProxy, logger); err != nil || shouldRequeue {
		return ctrl.Result{Requeue: shouldRequeue}, err
	}

	// Prepare a source path and get current commit hash
	appSourcePath, err := r.prepareSource(cdk8sAppProxy, logger)
	if err != nil {
		return ctrl.Result{}, err
	}

	parsedResources, err := r.synthesizeAndParseResources(appSourcePath, logger)
	if err != nil {
		logger.Error(err, "failed to synthesize and parse resources", "appSourcePath", appSourcePath)
	}

	if len(parsedResources) == 0 {
		if err := r.handleNoResources(ctx, cdk8sAppProxy, logger); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	// Determine if apply is needed
	clusterList, err := r.applyNeeded(ctx, cdk8sAppProxy, parsedResources, logger)
	if err != nil {
		logger.Error(err, "Failed to determine if apply is needed")
	}

	_, err = r.applyResourcesToClusters(ctx, cdk8sAppProxy, parsedResources, clusterList, proxyNamespacedName, logger)
	if err != nil {
		logger.Error(err, "Failed to apply resources to clusters", "appSourcePath", appSourcePath)

		return ctrl.Result{}, err
	}

	pollInterval := 5 * time.Minute
	if cdk8sAppProxy.Spec.GitRepository.ReferencePollInterval != nil {
		pollInterval = cdk8sAppProxy.Spec.GitRepository.ReferencePollInterval.Duration
	}

	repoUrl := cdk8sAppProxy.Spec.GitRepository.URL
	branch := cdk8sAppProxy.Spec.GitRepository.Reference
	buf := &bytes.Buffer{}
	polling := false

	ctx, _ = context.WithCancel(ctx)

	go func() {
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				logger.Info("Polling git repository for changes", "repoUrl", repoUrl, "branch", branch)
				polling, err = gitImpl.Poll(repoUrl, branch, appSourcePath, buf)
				if err != nil {
					logger.Error(err, "Polling git repository", "repoUrl", repoUrl, "branch", branch)
				}
				if polling {
					logger.Info("Detected changes in git repository, proceeding with reconciliation.")
					appSourcePath, err = r.prepareSource(cdk8sAppProxy, logger)
					if err != nil {
						logger.Error(err, "Prepare source for reconciliation")
					}

					if err = gitImpl.CleanUp(appSourcePath, 1*time.Minute); err != nil {
						logger.Error(err, "Failed to clean up old git repository clone directory", "appSourcePath", appSourcePath)
					}

					// Synthesize and parse resources
					_, err := r.synthesizeAndParseResources(appSourcePath, logger)
					if err != nil {
						logger.Error(err, "failed to synthesize and parse resources", "appSourcePath", appSourcePath)
					}

					parsedResources, err := r.synthesizeAndParseResources(appSourcePath, logger)
					if err != nil {
						logger.Error(err, "failed to synthesize and parse resources", "appSourcePath", appSourcePath)
					}

					if len(parsedResources) == 0 {
						if err := r.handleNoResources(ctx, cdk8sAppProxy, logger); err != nil {
							logger.Error(err, "Failed to handle no resources case")

							return
						}

						logger.Info("No valid Kubernetes resources parsed from manifest files, skipping apply.")
					}

					// Determine if apply is needed
					clusterList, err := r.applyNeeded(ctx, cdk8sAppProxy, parsedResources, logger)
					if err != nil {
						logger.Error(err, "Failed to determine if apply is needed")
					}

					_, err = r.applyResourcesToClusters(ctx, cdk8sAppProxy, parsedResources, clusterList, proxyNamespacedName, logger)
					if err != nil {
						logger.Error(err, "Failed to apply resources to clusters", "appSourcePath", appSourcePath)

						return
					}
				}
			case <-ctx.Done():
				logger.Info("Stopping git repository polling loop due to context cancellation.")

				return
			}
		}
	}()

	return ctrl.Result{}, nil
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

func (r *Reconciler) handleNoResources(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, logger logr.Logger) error {
	logger.Info("No valid Kubernetes resources parsed from manifest files")
	conditions.MarkFalse(cdk8sAppProxy, addonsv1alpha1.DeploymentProgressingCondition, addonsv1alpha1.NoResourcesParsedReason, clusterv1.ConditionSeverityWarning, "No valid Kubernetes resources found in manifests")
	if err := r.Status().Update(ctx, cdk8sAppProxy); err != nil {
		logger.Error(err, "Failed to update status after no resources parsed")

		return err
	}

	return nil
}

func (r *Reconciler) applyNeeded(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, parsedResources []*unstructured.Unstructured, logger logr.Logger) (clusterv1.ClusterList, error) {
	var clusterList clusterv1.ClusterList

	_, list, err := r.verifyResourcesOnClusters(ctx, cdk8sAppProxy, parsedResources, logger)
	if err != nil {
		return clusterList, err
	}
	clusterList = list

	return clusterList, nil
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

		return true, clusterList, r.updateStatusWithError(ctx, cdk8sAppProxy, addonsv1alpha1.ClusterSelectorParseFailedReason, "Failed to parse ClusterSelector for verification", err, false)
	}

	logger.Info("Listing clusters for resource verification", "selector", selector.String(), "namespace", cdk8sAppProxy.Namespace)
	if err := r.List(ctx, &clusterList, client.MatchingLabelsSelector{Selector: selector}); err != nil {
		logger.Error(err, "Failed to list clusters for verification, assuming resources might be missing.")

		return true, clusterList, r.updateStatusWithError(ctx, cdk8sAppProxy, addonsv1alpha1.ListClustersFailedReason, "Failed to list clusters for verification", err, false)
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
			exists, checkErr := r.checkIfResourceExists(ctx, dynamicClient, gvr, resource.GetNamespace(), resource.GetName())
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

//nolint:unparam // ctrl.Result is required for controller-runtime reconciler pattern
func (r *Reconciler) applyResourcesToClusters(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, parsedResources []*unstructured.Unstructured, clusterList clusterv1.ClusterList, proxyNamespacedName types.NamespacedName, logger logr.Logger) (ctrl.Result, error) {
	logger.Info("Proceeding with application of resources to target clusters.")

	// Ensure clusterList is populated if needed
	if len(clusterList.Items) == 0 && len(parsedResources) > 0 {
		logger.Info("Cluster list for apply phase is empty, re-listing.")
		selector, err := metav1.LabelSelectorAsSelector(&cdk8sAppProxy.Spec.ClusterSelector)
		if err != nil {
			logger.Error(err, "Failed to parse ClusterSelector for application phase", "selector", cdk8sAppProxy.Spec.ClusterSelector)

			return ctrl.Result{}, r.updateStatusWithError(ctx, cdk8sAppProxy, addonsv1alpha1.ClusterSelectorParseFailedReason, "Failed to parse ClusterSelector for application", err, false)
		}
		logger.Info("Listing clusters for application phase", "selector", selector.String(), "namespace", cdk8sAppProxy.Namespace)
		if err := r.List(ctx, &clusterList, client.MatchingLabelsSelector{Selector: selector}); err != nil {
			logger.Error(err, "Failed to list clusters for application phase")

			return ctrl.Result{}, r.updateStatusWithError(ctx, cdk8sAppProxy, addonsv1alpha1.ListClustersFailedReason, "Failed to list clusters for application", err, false)
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
		if cdk8sAppProxy.Spec.GitRepository != nil && cdk8sAppProxy.Spec.GitRepository.URL != "" {
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

				if err := r.WatchManager.StartWatch(ctx, dynamicClient, gvk, appliedResource.GetNamespace(), appliedResource.GetName(), proxyNamespacedName); err != nil {
					clusterLogger.Error(err, "Failed to start watch for applied resource")
				} else {
					clusterLogger.Info("Successfully started watch for applied resource")
				}
			}
		}
	}

	if !overallSuccess {
		logger.Error(firstErrorEncountered, "One or more errors occurred during resource application to clusters")

		return ctrl.Result{}, firstErrorEncountered
	}

	// If we reach here, the overallSuccess is true.
	if cdk8sAppProxy.Spec.GitRepository != nil && cdk8sAppProxy.Spec.GitRepository.URL != "" {
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
