/*
Copyright 2023 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	addonsv1alpha1 "github.com/PatrickLaabs/cluster-api-addon-provider-cdk8s/api/v1alpha1"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	plumbingtransport "github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	k8scorev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/go-git/go-git/v5/storage/memory"
)

const gitPollInterval = 1 * time.Minute

// CommandExecutor defines an interface for running external commands.
type CommandExecutor interface {
	CombinedOutput() ([]byte, error)
	SetDir(dir string)
}

// RealCmdRunner is a concrete implementation of CommandExecutor that runs actual commands.
type RealCmdRunner struct {
	Name          string
	Args          []string
	Dir           string
	CommanderFunc func(command string, args ...string) ([]byte, error)
}

func (rcr *RealCmdRunner) SetDir(dir string) {
	rcr.Dir = dir
}

func (rcr *RealCmdRunner) CombinedOutput() ([]byte, error) {
	if rcr.CommanderFunc != nil {
		return rcr.CommanderFunc(rcr.Name, rcr.Args...)
	}
	cmd := exec.Command(rcr.Name, rcr.Args...)
	if rcr.Dir != "" {
		cmd.Dir = rcr.Dir
	}

	return cmd.CombinedOutput()
}

var cmdRunnerFactory = func(name string, args ...string) CommandExecutor {
	return &RealCmdRunner{Name: name, Args: args}
}

// SetCommander allows tests to override the command runner.
func SetCommander(factory func(name string, args ...string) CommandExecutor) {
	cmdRunnerFactory = factory
}

// ResetCommander resets the command runner to the default real implementation.
func ResetCommander() {
	cmdRunnerFactory = func(name string, args ...string) CommandExecutor {
		return &RealCmdRunner{Name: name, Args: args}
	}
}

// pollGitRepository periodically checks the remote git repository for changes.
func (r *Cdk8sAppProxyReconciler) pollGitRepository(ctx context.Context, proxyName types.NamespacedName) {
	logger := log.FromContext(ctx).WithValues("cdk8sappproxy", proxyName.String(), "goroutine", "pollGitRepository")
	logger.Info("Starting git repository polling loop")

	ticker := time.NewTicker(gitPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			logger.Info("Polling git repository for changes")

			cdk8sAppProxy := &addonsv1alpha1.Cdk8sAppProxy{}
			if err := r.Get(ctx, proxyName, cdk8sAppProxy); err != nil {
				if apierrors.IsNotFound(err) {
					logger.Info("Cdk8sAppProxy resource not found, stopping polling.")
					return
				}
				logger.Error(err, "Failed to get Cdk8sAppProxy for polling")
				continue // Try again on the next tick
			}

			if cdk8sAppProxy.Spec.GitRepository == nil || cdk8sAppProxy.Spec.GitRepository.URL == "" {
				logger.Info("GitRepository not configured for this Cdk8sAppProxy, stopping polling.")
				return
			}

			gitSpec := cdk8sAppProxy.Spec.GitRepository
			repoURL := gitSpec.URL

			// Determine the reference name for LsRemote.
			// LsRemote typically expects fully qualified names like "refs/heads/main".
			// If a short branch name is given, we prepend "refs/heads/".
			// If a tag is given, "refs/tags/".
			// If a hash is given, LsRemote might not work as expected for "polling" the tip.
			// For now, we'll focus on branches.
			refName := plumbing.HEAD
			if gitSpec.Reference != "" {
				if plumbing.IsHash(gitSpec.Reference) {
					logger.Info("Polling a specific commit hash is not actively supported. The poller will check if the remote still has this hash, but it won't detect 'new' commits beyond this specific one. If you want to track a branch, please specify a branch name.", "reference", gitSpec.Reference)
					// We could attempt to see if this hash is still a valid ref, but LsRemote is more about discovering refs.
					// For now, if it's a hash, we might not get meaningful polling results in terms of "new" commits.
					// One option: try to resolve it. If it's a known commit, LsRemote might list it.
					// Another option: skip polling for specific commit hashes.
					// Let's try to use it directly, LsRemote might find it if it's a known ref on the server.
					refName = plumbing.ReferenceName(gitSpec.Reference) // This might not be correct for a raw hash.
					// A better approach for a raw hash might be to skip polling or handle it differently.
					// For this iteration, we'll log and proceed, understanding it might not be effective for raw hashes.
				} else if strings.HasPrefix(gitSpec.Reference, "refs/") {
					refName = plumbing.ReferenceName(gitSpec.Reference)
				} else if strings.Contains(gitSpec.Reference, "/") { // Heuristic for full ref names not starting with refs/
					refName = plumbing.ReferenceName(gitSpec.Reference)
				} else { // Assume short branch or tag name
					// This is a simplification. `git symbolic-ref` or similar logic would be more robust
					// to distinguish between branches and tags if not fully qualified.
					// For now, assume branches are more common for polling.
					logger.Info("Assuming Git reference is a branch name, prepending 'refs/heads/' for LsRemote.", "reference", gitSpec.Reference)
					refName = plumbing.NewBranchReferenceName(gitSpec.Reference) // e.g., refs/heads/main
				}
			}
			logger.Info("Attempting to LsRemote", "url", repoURL, "refName", refName.String())

			rem := git.NewRemote(memory.NewStorage(), &config.RemoteConfig{
				URLs: []string{repoURL},
			})

			auth := &http.BasicAuth{} // Default to empty auth
			if gitSpec.AuthSecretRef != nil {
				secret := &k8scorev1.Secret{}
				secretKey := client.ObjectKey{Namespace: cdk8sAppProxy.Namespace, Name: gitSpec.AuthSecretRef.Name}
				if err := r.Get(ctx, secretKey, secret); err != nil {
					logger.Error(err, "Failed to get auth secret for LsRemote")
					continue
				}
				username, userOk := secret.Data["username"]
				password, passOk := secret.Data["password"]
				if !userOk || !passOk {
					logger.Error(errors.New("auth secret missing username or password"), "secretName", secretKey.String())
					continue
				}
				auth.Username = string(username)
				auth.Password = string(password)
				logger.Info("Using credentials from AuthSecretRef for LsRemote")
			}

			refs, err := rem.ListContext(ctx, &git.ListOptions{
				Auth: auth, // Use configured auth
			})

			if err != nil {
				logger.Error(err, "Failed to LsRemote from git repository")
				if errors.Is(err, plumbingtransport.ErrAuthenticationRequired) || strings.Contains(err.Error(), "authentication required") {
					logger.Info("Authentication failed for LsRemote. Please check credentials.")
				}
				continue
			}

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
				// If HEAD was used and not found, try to find the default branch (e.g. main or master)
				// This is a common scenario if `gitSpec.Reference` was empty.
				if refName == plumbing.HEAD {
					logger.Info("HEAD reference not explicitly found, searching for default branches like main/master.")
					defaultBranches := []plumbing.ReferenceName{
						plumbing.NewBranchReferenceName("main"),
						plumbing.NewBranchReferenceName("master"),
					}
					for _, defaultBranchRef := range defaultBranches {
						for _, ref := range refs {
							if ref.Name() == defaultBranchRef {
								remoteCommitHash = ref.Hash().String()
								foundRef = true
								logger.Info("Found default branch reference", "refName", defaultBranchRef.String(), "remoteCommitHash", remoteCommitHash)
								break
							}
						}
						if foundRef {
							break
						}
					}
				}
			}

			if !foundRef {
				logger.Info("Specified reference not found in LsRemote output.", "refName", refName.String())
				// This could happen if the branch/tag does not exist or if `gitSpec.Reference` was a hash not advertised.
				// Or if the default HEAD resolution didn't find a common default branch.
				continue
			}

			if remoteCommitHash == "" {
				logger.Info("Remote commit hash is empty after LsRemote, skipping update check.")
				continue
			}

			// Get the Cdk8sAppProxy resource again to ensure we have the latest version for status update
			latestCdk8sAppProxy := &addonsv1alpha1.Cdk8sAppProxy{}
			if err := r.Get(ctx, proxyName, latestCdk8sAppProxy); err != nil {
				logger.Error(err, "Failed to get latest Cdk8sAppProxy before status update")
				continue
			}

			if latestCdk8sAppProxy.Status.LastRemoteGitHash != remoteCommitHash {
				logger.Info("Detected change in remote git repository", "oldHash", latestCdk8sAppProxy.Status.LastRemoteGitHash, "newHash", remoteCommitHash)
				latestCdk8sAppProxy.Status.LastRemoteGitHash = remoteCommitHash

				// Update status first
				if err := r.Status().Update(ctx, latestCdk8sAppProxy); err != nil {
					logger.Error(err, "Failed to update Cdk8sAppProxy status with new remote hash")
					continue // Try again next time
				}
				logger.Info("Successfully updated status with new remote hash. Now triggering reconciliation.")

				// Re-fetch to ensure we have the version with updated status before updating annotations
				proxyToAnnotate := &addonsv1alpha1.Cdk8sAppProxy{}
				if err := r.Get(ctx, proxyName, proxyToAnnotate); err != nil {
					logger.Error(err, "Failed to get latest Cdk8sAppProxy for annotation update")
					continue
				}

				// Trigger reconciliation by updating an annotation
				if proxyToAnnotate.Annotations == nil {
					proxyToAnnotate.Annotations = make(map[string]string)
				}
				proxyToAnnotate.Annotations["cdk8s.addons.cluster.x-k8s.io/git-poll-trigger"] = time.Now().Format(time.RFC3339Nano)
				if err := r.Update(ctx, proxyToAnnotate); err != nil {
					logger.Error(err, "Failed to update Cdk8sAppProxy annotations to trigger reconciliation")
					// If this fails, the status is updated, but reconciliation might be delayed.
					// The main reconcile loop should still pick up the change in LastRemoteGitHash vs LastProcessedGitHash.
				} else {
					logger.Info("Successfully updated annotations to trigger reconciliation.")
				}
			} else {
				logger.Info("No change detected in remote git repository.", "currentRemoteHash", remoteCommitHash)
			}

		case <-ctx.Done():
			logger.Info("Stopping git repository polling loop due to context cancellation.")
			return
		}
	}
}

const Cdk8sAppProxyFinalizer = "cdk8sappproxy.addons.cluster.x-k8s.io/finalizer"

// Cdk8sAppProxyReconciler reconciles a Cdk8sAppProxy object.
type Cdk8sAppProxyReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder // Add this line
	// Log      logr.Logger // Logger is available from context
	ActiveWatches    map[types.NamespacedName]map[string]context.CancelFunc
	activeGitPollers map[types.NamespacedName]context.CancelFunc
}

// gitProgressLogger buffers git progress messages and logs them line by line.
type gitProgressLogger struct {
	logger logr.Logger
	buffer []byte
}

// Write implements the io.Writer interface.
func (gpl *gitProgressLogger) Write(p []byte) (n int, err error) {
	gpl.buffer = append(gpl.buffer, p...)
	for {
		idx := bytes.IndexByte(gpl.buffer, '\n')
		if idx == -1 {
			// If buffer gets too large without a newline, log it to prevent OOM
			if len(gpl.buffer) > 1024 {
				gpl.logger.Info(strings.TrimSpace(string(gpl.buffer)))
				gpl.buffer = nil // Clear buffer
			}

			break
		}
		line := gpl.buffer[:idx]
		gpl.buffer = gpl.buffer[idx+1:]
		gpl.logger.Info(strings.TrimSpace(string(line))) // Log each full line
	}

	return len(p), nil
}

// checkIfResourceExists checks if a given resource exists on the target cluster.
// It uses a dynamic client to make the check.
// Parameters:
//   - ctx: Context for the request.
//   - dynClient: Dynamic Kubernetes client.
//   - gvr: GroupVersionResource of the resource to check.
//   - namespace: Namespace of the resource. Can be empty for cluster-scoped resources.
//   - name: Name of the resource.
//
// Returns:
//   - bool: True if the resource exists, false otherwise.
//   - error: An error if the check failed for reasons other than NotFound, nil otherwise.
func checkIfResourceExists(ctx context.Context, dynClient dynamic.Interface, gvr schema.GroupVersionResource, namespace string, name string) (bool, error) {
	resourceGetter := dynClient.Resource(gvr)
	if namespace != "" {
		_, err := resourceGetter.Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil // Resource does not exist
			}
			// Some other error occurred
			return false, errors.Wrapf(err, "failed to get namespaced resource %s/%s with GVR %s", namespace, name, gvr.String())
		}
	} else {
		_, err := resourceGetter.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil // Resource does not exist
			}
			// Some other error occurred
			return false, errors.Wrapf(err, "failed to get cluster-scoped resource %s with GVR %s", name, gvr.String())
		}
	}
	return true, nil // Resource exists
}

//+kubebuilder:rbac:groups=addons.cluster.x-k8s.io,resources=cdk8sappproxies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=addons.cluster.x-k8s.io,resources=cdk8sappproxies/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=addons.cluster.x-k8s.io,resources=cdk8sappproxies/finalizers,verbs=update
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;watch
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters/status,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *Cdk8sAppProxyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
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

func (r *Cdk8sAppProxyReconciler) reconcileNormal(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("cdk8sappproxy", types.NamespacedName{Name: cdk8sAppProxy.Name, Namespace: cdk8sAppProxy.Namespace}, "reconcile_type", "normal")
	logger.Info("Starting reconcileNormal")

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
				cdk8sAppProxy.Annotations = nil // Explicitly set to nil if empty
			}

			if err := r.Update(ctx, cdk8sAppProxy); err != nil {
				logger.Error(err, "Failed to clear the resource deletion trigger annotation. Requeuing.", "annotationKey", deletionTriggerAnnotationKey)
				return ctrl.Result{}, err // Requeue to retry clearing the annotation
			}
			logger.Info("Successfully cleared the resource deletion trigger annotation.", "annotationKey", deletionTriggerAnnotationKey)
		}
	}
	var currentCommitHash string

	// Get the NamespacedName for the Cdk8sAppProxy resource
	proxyNamespacedName := types.NamespacedName{Name: cdk8sAppProxy.Name, Namespace: cdk8sAppProxy.Namespace}

	// Ensure ActiveWatches sub-map is initialized for this specific proxy
	// This needs to be done after cdk8sAppProxy is fetched and proxyNamespacedName is defined.
	if r.ActiveWatches[proxyNamespacedName] == nil {
		r.ActiveWatches[proxyNamespacedName] = make(map[string]context.CancelFunc)
	}

	if !controllerutil.ContainsFinalizer(cdk8sAppProxy, Cdk8sAppProxyFinalizer) {
		logger.Info("Adding finalizer", "finalizer", Cdk8sAppProxyFinalizer)
		controllerutil.AddFinalizer(cdk8sAppProxy, Cdk8sAppProxyFinalizer)
		if err := r.Update(ctx, cdk8sAppProxy); err != nil {
			logger.Error(err, "Failed to add finalizer")

			return ctrl.Result{}, err
		}
		logger.Info("Successfully added finalizer")

		return ctrl.Result{Requeue: true}, nil
	}

	var appSourcePath string
	var cleanupFunc func()
	defer func() {
		if cleanupFunc != nil {
			logger.Info("Cleaning up temporary source directory")
			cleanupFunc()
		}
	}()

	if cdk8sAppProxy.Spec.GitRepository != nil && cdk8sAppProxy.Spec.GitRepository.URL != "" {
		gitSpec := cdk8sAppProxy.Spec.GitRepository
		logger.Info("Determined source type: GitRepository", "url", gitSpec.URL, "reference", gitSpec.Reference, "path", gitSpec.Path)
		tempDir, err := os.MkdirTemp("", "cdk8s-git-clone-")
		if err != nil {
			logger.Error(err, "Failed to create temp directory for git clone")
			_ = r.handleError(ctx, cdk8sAppProxy, addonsv1alpha1.GitCloneFailedReason, "Failed to create temp dir for git clone", err)

			return ctrl.Result{}, err // Or return specific error
		}
		cleanupFunc = func() {
			logger.Info("Removing temporary clone directory", "tempDir", tempDir)
			if err := os.RemoveAll(tempDir); err != nil {
				logger.Error(err, "Failed to remove temporary clone directory", "tempDir", tempDir)
			}
		}
		logger.Info("Created temporary directory for clone", "tempDir", tempDir)

		cloneOptions := &git.CloneOptions{
			URL:      gitSpec.URL,
			Progress: &gitProgressLogger{logger: logger.WithName("git-clone")},
		}

		if gitSpec.AuthSecretRef != nil {
			logger.Info("AuthSecretRef specified, attempting to fetch secret", "secretName", gitSpec.AuthSecretRef.Name)
			authSecret := &k8scorev1.Secret{}
			secretKey := client.ObjectKey{Namespace: cdk8sAppProxy.Namespace, Name: gitSpec.AuthSecretRef.Name}
			if err := r.Get(ctx, secretKey, authSecret); err != nil {
				logger.Error(err, "Failed to get auth secret for git clone")
				_ = r.handleError(ctx, cdk8sAppProxy, addonsv1alpha1.GitAuthSecretInvalidReason, "Failed to get auth secret "+secretKey.String(), err)

				return ctrl.Result{}, err
			}

			username, okUser := authSecret.Data["username"]
			password, okPass := authSecret.Data["password"]

			if !okUser || !okPass {
				err := errors.New("auth secret missing username or password fields")
				logger.Error(err, "Invalid auth secret", "secretName", secretKey.String())
				_ = r.handleError(ctx, cdk8sAppProxy, addonsv1alpha1.GitAuthSecretInvalidReason, err.Error(), err)

				return ctrl.Result{}, err
			}
			logger.Info("Successfully fetched auth secret and found credentials")
			cloneOptions.Auth = &http.BasicAuth{
				Username: string(username),
				Password: string(password),
			}
		}

		logger.Info("Executing git clone with go-git", "url", gitSpec.URL, "targetDir", tempDir)
		_, err = git.PlainCloneContext(ctx, tempDir, false, cloneOptions)
		if err != nil {
			logger.Error(err, "go-git PlainCloneContext failed")
			reason := addonsv1alpha1.GitCloneFailedReason
			if errors.Is(err, plumbingtransport.ErrAuthenticationRequired) || strings.Contains(err.Error(), "authentication required") {
				reason = addonsv1alpha1.GitAuthenticationFailedReason
			}
			_ = r.handleError(ctx, cdk8sAppProxy, reason, "go-git clone failed", err)

			return ctrl.Result{}, err
		}
		logger.Info("Successfully cloned git repository with go-git")

		if gitSpec.Reference != "" {
			logger.Info("Executing git checkout with go-git", "reference", gitSpec.Reference, "dir", tempDir)
			repo, err := git.PlainOpen(tempDir)
			if err != nil {
				logger.Error(err, "go-git PlainOpen failed")
				_ = r.handleError(ctx, cdk8sAppProxy, addonsv1alpha1.GitCheckoutFailedReason, "go-git PlainOpen failed", err)

				return ctrl.Result{}, err
			}

			worktree, err := repo.Worktree()
			if err != nil {
				logger.Error(err, "go-git Worktree failed")
				_ = r.handleError(ctx, cdk8sAppProxy, addonsv1alpha1.GitCheckoutFailedReason, "go-git Worktree failed", err)

				return ctrl.Result{}, err
			}

			checkoutOpts := &git.CheckoutOptions{Force: true} // Equivalent to -f
			if plumbing.IsHash(gitSpec.Reference) {
				checkoutOpts.Hash = plumbing.NewHash(gitSpec.Reference)
			} else {
				// Try to resolve as a branch or tag name
				// plumbing.ReferenceName needs to be fully qualified, e.g. refs/heads/main or refs/tags/v1.0.0
				// For short names like "main" or "v1.0.0", ResolveRevision is more robust.
				revision := plumbing.Revision(gitSpec.Reference)
				resolvedHash, err := repo.ResolveRevision(revision)
				if err != nil {
					logger.Error(err, "go-git ResolveRevision failed", "reference", gitSpec.Reference)
					_ = r.handleError(ctx, cdk8sAppProxy, addonsv1alpha1.GitCheckoutFailedReason, "go-git ResolveRevision failed for ref "+gitSpec.Reference, err)

					return ctrl.Result{}, err
				}
				checkoutOpts.Hash = *resolvedHash // Checkout the resolved hash
			}

			err = worktree.Checkout(checkoutOpts)
			if err != nil {
				logger.Error(err, "go-git Checkout failed", "reference", gitSpec.Reference)
				_ = r.handleError(ctx, cdk8sAppProxy, addonsv1alpha1.GitCheckoutFailedReason, "go-git Checkout failed for ref "+gitSpec.Reference, err)

				return ctrl.Result{}, err
			}
			logger.Info("Successfully checked out git reference with go-git", "reference", gitSpec.Reference)
		}
		appSourcePath = tempDir
		if gitSpec.Path != "" {
			appSourcePath = filepath.Join(tempDir, gitSpec.Path)
			logger.Info("Adjusted appSourcePath for repository subpath", "subPath", gitSpec.Path, "finalPath", appSourcePath)
		}

		// Attempt to retrieve current commit hash
		logger.Info("Attempting to retrieve current commit hash from Git repository", "repoDir", tempDir)
		repo, err := git.PlainOpen(tempDir)
		if err != nil {
			logger.Error(err, "Failed to open git repository after clone/checkout")
			_ = r.handleError(ctx, cdk8sAppProxy, addonsv1alpha1.GitCloneFailedReason, "Failed to open git repository: "+err.Error(), err)
			return ctrl.Result{}, err
		}
		headRef, err := repo.Head()
		if err != nil {
			logger.Error(err, "Failed to get HEAD reference from git repository")
			_ = r.handleError(ctx, cdk8sAppProxy, addonsv1alpha1.GitCloneFailedReason, "Failed to get HEAD reference: "+err.Error(), err)
			return ctrl.Result{}, err
		}
		currentCommitHash = headRef.Hash().String()
		logger.Info("Successfully retrieved current commit hash from Git repository", "commitHash", currentCommitHash)

		// Store current commit hash in status
		if currentCommitHash != "" {
			cdk8sAppProxy.Status.LastRemoteGitHash = currentCommitHash
			// Note: Status update will happen later in the reconciliation logic
			logger.Info("Updated cdk8sAppProxy.Status.LastRemoteGitHash with the latest commit hash from remote", "lastRemoteGitHash", currentCommitHash)
		}

	} else if cdk8sAppProxy.Spec.LocalPath != "" {
		logger.Info("Determined source type: LocalPath", "path", cdk8sAppProxy.Spec.LocalPath)
		appSourcePath = cdk8sAppProxy.Spec.LocalPath
		// If GitRepository is not configured, ensure any existing poller for this proxy is stopped.
		if cancel, ok := r.activeGitPollers[proxyNamespacedName]; ok {
			logger.Info("GitRepository spec removed or empty, stopping existing git poller.")
			cancel()
			delete(r.activeGitPollers, proxyNamespacedName)
		}
	} else {
		err := errors.New("no source specified")
		logger.Error(err, "No source specified (neither GitRepository nor LocalPath)")
		// Ensure any existing poller is stopped if the spec becomes invalid
		if cancel, ok := r.activeGitPollers[proxyNamespacedName]; ok {
			logger.Info("Source spec is invalid or removed, stopping existing git poller.")
			cancel()
			delete(r.activeGitPollers, proxyNamespacedName)
		}
		_ = r.handleError(ctx, cdk8sAppProxy, addonsv1alpha1.SourceNotSpecifiedReason, "Neither GitRepository nor LocalPath specified", err)

		return ctrl.Result{}, err
	}

	// Manage Git Poller Lifecycle
	if cdk8sAppProxy.Spec.GitRepository != nil && cdk8sAppProxy.Spec.GitRepository.URL != "" {
		if _, pollerExists := r.activeGitPollers[proxyNamespacedName]; !pollerExists {
			logger.Info("Starting new git poller.")
			pollCtx, cancelPoll := context.WithCancel(context.Background()) // Use Background for long-running goroutine
			r.activeGitPollers[proxyNamespacedName] = cancelPoll
			go r.pollGitRepository(pollCtx, proxyNamespacedName)
		} else {
			logger.Info("Git poller already active.")
			// For now, we don't restart the poller if spec changes, poller will pick up changes.
			// A more advanced implementation might compare old and new gitSpec and restart if necessary.
		}
	} else {
		// This case is also handled above when appSourcePath is determined for LocalPath,
		// but it's good to have an explicit check here as well.
		if cancel, pollerExists := r.activeGitPollers[proxyNamespacedName]; pollerExists {
			logger.Info("GitRepository is not configured, ensuring poller is stopped.")
			cancel()
			delete(r.activeGitPollers, proxyNamespacedName)
		}
	}

	logger.Info("Synthesizing cdk8s application", "effectiveSourcePath", appSourcePath)
	synthCmd := cmdRunnerFactory("cdk8s", "synth")
	synthCmd.SetDir(appSourcePath)
	output, synthErr := synthCmd.CombinedOutput()
	if synthErr != nil {
		logger.Error(synthErr, "cdk8s synth failed", "output", string(output))
		_ = r.handleError(ctx, cdk8sAppProxy, addonsv1alpha1.Cdk8sSynthFailedReason, "cdk8s synth failed. Output: "+string(output), synthErr)

		return ctrl.Result{}, synthErr
	}
	logger.Info("cdk8s synth successful", "outputSummary", truncateString(string(output), 200))
	logger.V(1).Info("cdk8s synth full output", "output", string(output)) // Log full output at V(1)

	distPath := filepath.Join(appSourcePath, "dist")
	logger.Info("Looking for synthesized manifests", "distPath", distPath)
	var manifestFiles []string
	walkErr := filepath.WalkDir(distPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && (strings.HasSuffix(d.Name(), ".yaml") || strings.HasSuffix(d.Name(), ".yml")) {
			manifestFiles = append(manifestFiles, path)
		}

		return nil
	})
	if walkErr != nil {
		logger.Error(walkErr, "Failed to walk dist directory")
		_ = r.handleError(ctx, cdk8sAppProxy, addonsv1alpha1.WalkDistFailedReason, "Failed to walk dist directory", walkErr)

		return ctrl.Result{}, walkErr
	}
	if len(manifestFiles) == 0 {
		logger.Info("No manifest files found in dist directory")
		conditions.MarkFalse(cdk8sAppProxy, addonsv1alpha1.DeploymentProgressingCondition, addonsv1alpha1.NoManifestsFoundReason, clusterv1.ConditionSeverityWarning, "No YAML manifests found in dist directory")
		if err := r.Status().Update(ctx, cdk8sAppProxy); err != nil {
			logger.Error(err, "Failed to update status after no manifests found")
		}

		return ctrl.Result{}, nil // No manifests, so nothing to process or apply
	}
	logger.Info("Found manifest files", "count", len(manifestFiles), "files", manifestFiles)

	var parsedResources []*unstructured.Unstructured
	for _, manifestFile := range manifestFiles {
		logger.Info("Processing manifest file", "file", manifestFile)
		fileContent, readErr := os.ReadFile(manifestFile)
		if readErr != nil {
			logger.Error(readErr, "Failed to read manifest file", "file", manifestFile)
			_ = r.handleError(ctx, cdk8sAppProxy, addonsv1alpha1.ReadManifestFailedReason, "Failed to read manifest "+manifestFile, readErr)

			return ctrl.Result{}, readErr
		}
		yamlDecoder := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(fileContent), 100)
		for {
			var rawObj runtime.RawExtension
			if err := yamlDecoder.Decode(&rawObj); err != nil {
				if err.Error() == "EOF" {
					break
				}
				logger.Error(err, "Failed to decode YAML from manifest file", "file", manifestFile)

				_ = r.handleError(ctx, cdk8sAppProxy, addonsv1alpha1.DecodeManifestFailedReason, "Failed to decode YAML from "+manifestFile, err)

				return ctrl.Result{}, err
			}
			if rawObj.Raw == nil {
				continue
			}
			u := &unstructured.Unstructured{}
			if _, _, err := unstructured.UnstructuredJSONScheme.Decode(rawObj.Raw, nil, u); err != nil {
				logger.Error(err, "Failed to decode RawExtension to Unstructured", "file", manifestFile)
				_ = r.handleError(ctx, cdk8sAppProxy, addonsv1alpha1.DecodeToUnstructuredFailedReason, "Failed to decode to Unstructured from "+manifestFile, err)

				return ctrl.Result{}, err
			}
			logger.Info("Parsed resource", "file", manifestFile, "GVK", u.GroupVersionKind().String(), "Name", u.GetName(), "Namespace", u.GetNamespace())
			parsedResources = append(parsedResources, u)
		}
	}
	if len(parsedResources) == 0 {
		logger.Info("No valid Kubernetes resources parsed from manifest files")
		conditions.MarkFalse(cdk8sAppProxy, addonsv1alpha1.DeploymentProgressingCondition, addonsv1alpha1.NoResourcesParsedReason, clusterv1.ConditionSeverityWarning, "No valid Kubernetes resources found in manifests")
		if err := r.Status().Update(ctx, cdk8sAppProxy); err != nil {
			logger.Error(err, "Failed to update status after no resources parsed")
		}
		// If no resources are parsed, it's similar to no manifests found.
		// We might want to update status and return.
		// Depending on desired behavior, this could be an error or just an empty result.
		// For now, let's match the "No manifest files found" behavior.
		return ctrl.Result{}, nil
	}
	logger.Info("Successfully parsed all resources from manifest files", "count", len(parsedResources))

	// Change Detection Logic
	triggeredByGitOrAnnotation := false // Default to false, explicit conditions will set it to true

	// Condition for periodic git poller
	if cdk8sAppProxy.Status.LastRemoteGitHash != "" &&
		cdk8sAppProxy.Status.LastRemoteGitHash != cdk8sAppProxy.Status.LastProcessedGitHash &&
		cdk8sAppProxy.Status.LastRemoteGitHash != currentCommitHash { // currentCommitHash is from the fresh clone
		triggeredByGitOrAnnotation = true
		logger.Info("Reconciliation proceeding due to change detected by git poller.",
			"lastRemoteGitHash", cdk8sAppProxy.Status.LastRemoteGitHash,
			"lastProcessedGitHash", cdk8sAppProxy.Status.LastProcessedGitHash,
			"currentCommitHash", currentCommitHash)
	}

	if !triggeredByGitOrAnnotation && cdk8sAppProxy.Spec.GitRepository != nil && cdk8sAppProxy.Spec.GitRepository.URL != "" {
		if currentCommitHash == "" {
			logger.Info("currentCommitHash is unexpectedly empty for Git source; proceeding with update as a precaution.")
			triggeredByGitOrAnnotation = true
		} else {
			lastProcessedGitHash := cdk8sAppProxy.Status.LastProcessedGitHash
			gitSpecRef := cdk8sAppProxy.Spec.GitRepository.Reference // for logging
			repositoryHasChanged := currentCommitHash != lastProcessedGitHash
			isInitialDeployment := lastProcessedGitHash == ""

			if isInitialDeployment {
				logger.Info("Initial deployment or no last processed hash found. Proceeding with cdk8s synth and apply.", "currentCommitHash", currentCommitHash, "reference", gitSpecRef)
				triggeredByGitOrAnnotation = true
			} else if repositoryHasChanged {
				logger.Info("Git repository has changed (current clone vs last processed), proceeding with update.", "currentCommitHash", currentCommitHash, "lastProcessedGitHash", lastProcessedGitHash, "reference", gitSpecRef)
				triggeredByGitOrAnnotation = true
			} else {
				logger.Info("No new Git changes detected (current clone matches last processed, and no pending poller detection).", "commitHash", currentCommitHash, "reference", gitSpecRef)
				// triggeredByGitOrAnnotation remains false
			}
		}
	} else if !triggeredByGitOrAnnotation && (cdk8sAppProxy.Spec.LocalPath != "" || (cdk8sAppProxy.Spec.GitRepository == nil || cdk8sAppProxy.Spec.GitRepository.URL == "")) {
		if cdk8sAppProxy.Spec.LocalPath != "" && cdk8sAppProxy.Status.ObservedGeneration == 0 {
			logger.Info("Initial processing for LocalPath or source type without explicit change detection. Proceeding with cdk8s synth and apply.")
			triggeredByGitOrAnnotation = true
		} else if cdk8sAppProxy.Spec.LocalPath != "" {
			logger.Info("Subsequent reconciliation for LocalPath and no explicit trigger found. Will check resource integrity.")
		}
	}

	if forceSynthAndApplyDueToDeletion {
		if !triggeredByGitOrAnnotation {
			logger.Info("Forcing synth and apply because reconciliation was triggered by a resource deletion, overriding previous checks if they suggested no change.", "annotationKey", deletionTriggerAnnotationKey)
		}
		triggeredByGitOrAnnotation = true
	}

	applyNeeded := triggeredByGitOrAnnotation
	var clusterListForApply clusterv1.ClusterList // Define clusterListForApply here to potentially reuse if verification lists clusters

	if !applyNeeded {
		logger.Info("No primary triggers (Git changes/Annotation) for resource application. Starting resource verification.")
		foundMissingResourcesOnAnyCluster := false
		if len(parsedResources) == 0 {
			logger.Info("No parsed resources to verify. Skipping resource verification.")
		} else {
			selector, err := metav1.LabelSelectorAsSelector(&cdk8sAppProxy.Spec.ClusterSelector)
			if err != nil {
				logger.Error(err, "Failed to parse ClusterSelector for verification, assuming resources might be missing.", "selector", cdk8sAppProxy.Spec.ClusterSelector)
				_ = r.handleError(ctx, cdk8sAppProxy, addonsv1alpha1.ClusterSelectorParseFailedReason, "Failed to parse ClusterSelector for verification", err) // Update status
				foundMissingResourcesOnAnyCluster = true                                                                                                        // Force apply due to error
			} else {
				logger.Info("Listing clusters for resource verification", "selector", selector.String(), "namespace", cdk8sAppProxy.Namespace)
				if err := r.List(ctx, &clusterListForApply, client.MatchingLabelsSelector{Selector: selector}); err != nil {
					logger.Error(err, "Failed to list clusters for verification, assuming resources might be missing.")
					_ = r.handleError(ctx, cdk8sAppProxy, addonsv1alpha1.ListClustersFailedReason, "Failed to list clusters for verification", err) // Update status
					foundMissingResourcesOnAnyCluster = true                                                                                        // Force apply due to error
				} else if len(clusterListForApply.Items) == 0 {
					logger.Info("No clusters found matching selector for verification. Skipping resource verification on clusters.")
				} else {
					logger.Info("Successfully listed clusters for verification", "count", len(clusterListForApply.Items))
					for _, cluster := range clusterListForApply.Items {
						clusterLogger := logger.WithValues("targetCluster", cluster.Name)
						clusterLogger.Info("Verifying resources on cluster")
						dynamicClient, err := r.getDynamicClientForCluster(ctx, cluster.Namespace, cluster.Name)
						if err != nil {
							clusterLogger.Error(err, "Failed to get dynamic client for verification. Assuming resources missing on this cluster.")
							foundMissingResourcesOnAnyCluster = true
							break // Stop checking this cluster, move to forcing apply
						}
						for _, resource := range parsedResources {
							gvr := resource.GroupVersionKind().GroupVersion().WithResource(getPluralFromKind(resource.GetKind()))
							exists, checkErr := checkIfResourceExists(ctx, dynamicClient, gvr, resource.GetNamespace(), resource.GetName())
							if checkErr != nil {
								clusterLogger.Error(checkErr, "Error checking resource existence. Assuming resource missing.", "resourceName", resource.GetName(), "GVK", gvr)
								foundMissingResourcesOnAnyCluster = true
								break // Stop checking resources on this cluster
							}
							if !exists {
								clusterLogger.Info("Resource missing on target cluster.", "resourceName", resource.GetName(), "GVK", gvr, "namespace", resource.GetNamespace())
								foundMissingResourcesOnAnyCluster = true
								break // Stop checking resources on this cluster
							}
						}
						if foundMissingResourcesOnAnyCluster {
							break // Stop checking other clusters
						}
					}
				}
			}
		}

		if foundMissingResourcesOnAnyCluster {
			logger.Info("Resource verification detected missing resources. Forcing application of all resources.")
			applyNeeded = true
		} else {
			logger.Info("Resource verification complete. All declared resources appear to be present on target clusters.")
		}
	}

	if !applyNeeded {
		logger.Info("Skipping resource application: no Git changes, no deletion annotation, and all resources verified present.")
		cdk8sAppProxy.Status.ObservedGeneration = cdk8sAppProxy.Generation
		conditions.MarkTrue(cdk8sAppProxy, addonsv1alpha1.DeploymentProgressingCondition)
		if cdk8sAppProxy.Spec.GitRepository != nil && cdk8sAppProxy.Spec.GitRepository.URL != "" && currentCommitHash != "" {
			cdk8sAppProxy.Status.LastProcessedGitHash = currentCommitHash
			logger.Info("Updated LastProcessedGitHash to current commit hash as no changes or missing resources were found.", "hash", currentCommitHash)
		}
		if err := r.Status().Update(ctx, cdk8sAppProxy); err != nil {
			logger.Error(err, "Failed to update status after skipping resource application.")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// If applyNeeded is true:
	logger.Info("Proceeding with application of resources to target clusters.")

	// Ensure clusterListForApply is populated if verification didn't run or didn't populate it.
	// This check is a bit redundant if verification always populates it on success,
	// but good for safety if verification is skipped (e.g., len(parsedResources) == 0)
	// or if it failed early before listing clusters.
	if len(clusterListForApply.Items) == 0 && len(parsedResources) > 0 { // Only list if there are resources to apply
		logger.Info("Cluster list for apply phase is empty or not populated by verification, re-listing.")
		selector, err := metav1.LabelSelectorAsSelector(&cdk8sAppProxy.Spec.ClusterSelector)
		if err != nil {
			logger.Error(err, "Failed to parse ClusterSelector for application phase", "selector", cdk8sAppProxy.Spec.ClusterSelector)
			return ctrl.Result{}, r.handleError(ctx, cdk8sAppProxy, addonsv1alpha1.ClusterSelectorParseFailedReason, "Failed to parse ClusterSelector for application", err)
		}
		logger.Info("Listing clusters for application phase", "selector", selector.String(), "namespace", cdk8sAppProxy.Namespace)
		if err := r.List(ctx, &clusterListForApply, client.MatchingLabelsSelector{Selector: selector}); err != nil {
			logger.Error(err, "Failed to list clusters for application phase")
			return ctrl.Result{}, r.handleError(ctx, cdk8sAppProxy, addonsv1alpha1.ListClustersFailedReason, "Failed to list clusters for application", err)
		}
		if len(clusterListForApply.Items) == 0 {
			logger.Info("No clusters found matching the selector for application phase.")
			conditions.MarkFalse(cdk8sAppProxy, addonsv1alpha1.DeploymentProgressingCondition, addonsv1alpha1.NoMatchingClustersReason, clusterv1.ConditionSeverityInfo, "No clusters found matching selector for application")
			if errStatusUpdate := r.Status().Update(ctx, cdk8sAppProxy); errStatusUpdate != nil {
				logger.Error(errStatusUpdate, "Failed to update status when no matching clusters found for application")
			}
			return ctrl.Result{}, nil // No clusters to apply to
		}
		logger.Info("Successfully listed clusters for application phase", "count", len(clusterListForApply.Items))
	} else if len(parsedResources) == 0 {
		logger.Info("No parsed resources to apply, skipping application to clusters.")
		// This case should ideally be caught earlier (e.g. after synth), but as a safeguard:
		cdk8sAppProxy.Status.ObservedGeneration = cdk8sAppProxy.Generation
		conditions.MarkTrue(cdk8sAppProxy, addonsv1alpha1.DeploymentProgressingCondition) // No resources means "applied"
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

	for _, cluster := range clusterListForApply.Items {
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
				watchCtx, cancelWatch := context.WithCancel(context.Background())
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
		// Status already marked by handleError or within the loop by conditions.MarkFalse
		// We might not need to call handleError explicitly here if conditions are set correctly inside.
		// However, ensuring the top-level error is returned is important.
		return ctrl.Result{}, firstErrorEncountered
	}

	// If we reach here, overallSuccess is true.
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

func (r *Cdk8sAppProxyReconciler) reconcileDelete(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("cdk8sappproxy", types.NamespacedName{Name: cdk8sAppProxy.Name, Namespace: cdk8sAppProxy.Namespace}, "reconcile_type", "delete")
	logger.Info("Starting reconcileDelete")

	proxyNamespacedName := types.NamespacedName{Name: cdk8sAppProxy.Name, Namespace: cdk8sAppProxy.Namespace}

	// Stop any active git poller for this resource
	if cancel, ok := r.activeGitPollers[proxyNamespacedName]; ok {
		logger.Info("Stopping git poller due to resource deletion.")
		cancel()
		delete(r.activeGitPollers, proxyNamespacedName)
	}

	if !controllerutil.ContainsFinalizer(cdk8sAppProxy, Cdk8sAppProxyFinalizer) {
		logger.Info("Finalizer already removed, nothing to do.")

		return ctrl.Result{}, nil
	}

	var appSourcePathForDelete string
	var cleanupDeleteFunc func()
	defer func() {
		if cleanupDeleteFunc != nil {
			logger.Info("Cleaning up temporary source directory for deletion")
			cleanupDeleteFunc()
		}
	}()

	if cdk8sAppProxy.Spec.GitRepository != nil && cdk8sAppProxy.Spec.GitRepository.URL != "" {
		gitSpec := cdk8sAppProxy.Spec.GitRepository
		logger.Info("Using GitRepository source for deletion logic", "url", gitSpec.URL, "reference", gitSpec.Reference, "path", gitSpec.Path)
		tempDir, err := os.MkdirTemp("", "cdk8s-git-delete-")
		if err != nil {
			logger.Error(err, "failed to create temp dir for git clone during deletion")

			return ctrl.Result{}, err
		}
		cleanupDeleteFunc = func() {
			logger.Info("Removing temporary clone directory for deletion", "tempDir", tempDir)
			if err := os.RemoveAll(tempDir); err != nil {
				logger.Error(err, "Failed to remove temporary clone directory for deletion", "tempDir", tempDir)
			}
		}
		logger.Info("Created temporary directory for clone during deletion", "tempDir", tempDir)

		cloneOptionsDelete := &git.CloneOptions{
			URL:      gitSpec.URL,
			Progress: &gitProgressLogger{logger: logger.WithName("git-clone-delete")},
		}

		if gitSpec.AuthSecretRef != nil {
			logger.Info("AuthSecretRef specified for deletion clone, attempting to fetch secret", "secretName", gitSpec.AuthSecretRef.Name)
			authSecret := &k8scorev1.Secret{}
			secretKey := client.ObjectKey{Namespace: cdk8sAppProxy.Namespace, Name: gitSpec.AuthSecretRef.Name}
			if err := r.Get(ctx, secretKey, authSecret); err != nil {
				logger.Error(err, "Failed to get auth secret for git clone during deletion", "secretName", secretKey.String())

				return ctrl.Result{}, err
			}

			username, okUser := authSecret.Data["username"]
			password, okPass := authSecret.Data["password"]

			if !okUser || !okPass {
				err := errors.New("auth secret missing username or password fields for deletion clone")
				logger.Error(err, "Invalid auth secret for deletion clone", "secretName", secretKey.String())

				return ctrl.Result{}, err
			}
			logger.Info("Successfully fetched auth secret for deletion clone")
			cloneOptionsDelete.Auth = &http.BasicAuth{
				Username: string(username),
				Password: string(password),
			}
		}

		logger.Info("Executing git clone with go-git for deletion", "url", gitSpec.URL, "targetDir", tempDir)
		_, err = git.PlainCloneContext(ctx, tempDir, false, cloneOptionsDelete)
		if err != nil {
			logger.Error(err, "go-git PlainCloneContext failed during deletion")
			errMsg := "go-git clone failed during deletion"
			reason := addonsv1alpha1.GitCloneFailedReason
			if errors.Is(err, plumbingtransport.ErrAuthenticationRequired) || strings.Contains(err.Error(), "authentication required") {
				errMsg = "go-git authentication failed during deletion"
				reason = addonsv1alpha1.GitAuthenticationFailedReason
			}
			logger.Error(err, errMsg, "reason", reason)

			return ctrl.Result{}, err
		}
		logger.Info("Successfully cloned git repository with go-git for deletion")

		if gitSpec.Reference != "" {
			logger.Info("Executing git checkout with go-git for deletion", "reference", gitSpec.Reference, "dir", tempDir)
			repo, err := git.PlainOpen(tempDir)
			if err != nil {
				logger.Error(err, "go-git PlainOpen failed during deletion")

				return ctrl.Result{}, err
			}

			worktree, err := repo.Worktree()
			if err != nil {
				logger.Error(err, "go-git Worktree failed during deletion")

				return ctrl.Result{}, err
			}

			checkoutOpts := &git.CheckoutOptions{Force: true} // Equivalent to -f
			if plumbing.IsHash(gitSpec.Reference) {
				checkoutOpts.Hash = plumbing.NewHash(gitSpec.Reference)
			} else {
				revision := plumbing.Revision(gitSpec.Reference)
				resolvedHash, err := repo.ResolveRevision(revision)
				if err != nil {
					logger.Error(err, "go-git ResolveRevision failed during deletion", "reference", gitSpec.Reference)

					return ctrl.Result{}, err
				}
				checkoutOpts.Hash = *resolvedHash
			}

			err = worktree.Checkout(checkoutOpts)
			if err != nil {
				logger.Error(err, "go-git Checkout failed during deletion", "reference", gitSpec.Reference)

				return ctrl.Result{}, err
			}
			logger.Info("Successfully checked out git reference with go-git for deletion", "reference", gitSpec.Reference)
		}
		appSourcePathForDelete = tempDir
		if gitSpec.Path != "" {
			appSourcePathForDelete = filepath.Join(tempDir, gitSpec.Path)
			logger.Info("Adjusted appSourcePath for deletion", "subPath", gitSpec.Path, "finalPath", appSourcePathForDelete)
		}
	} else if cdk8sAppProxy.Spec.LocalPath != "" {
		logger.Info("Using LocalPath source for deletion logic", "path", cdk8sAppProxy.Spec.LocalPath)
		appSourcePathForDelete = cdk8sAppProxy.Spec.LocalPath
	} else {
		err := errors.New("neither GitRepository nor LocalPath specified, cannot determine resources to delete")
		logger.Info(err.Error())
		_ = r.handleDeleteError(ctx, cdk8sAppProxy, err.Error(), nil)

		return ctrl.Result{}, err
	}

	logger.Info("Synthesizing cdk8s application to identify resources for deletion", "effectiveSourcePath", appSourcePathForDelete)
	synthCmd := cmdRunnerFactory("cdk8s", "synth")
	synthCmd.SetDir(appSourcePathForDelete)
	output, synthErr := synthCmd.CombinedOutput()
	if synthErr != nil {
		logger.Error(synthErr, "cdk8s synth failed during deletion", "output", string(output))

		return ctrl.Result{}, synthErr
	}
	logger.Info("cdk8s synth successful for deletion", "outputSummary", truncateString(string(output), 200))
	logger.V(1).Info("cdk8s synth full output for deletion", "output", string(output))

	distPath := filepath.Join(appSourcePathForDelete, "dist")
	logger.Info("Looking for manifests for deletion", "distPath", distPath)
	var manifestFiles []string

	walkErr := filepath.WalkDir(distPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && (strings.HasSuffix(d.Name(), ".yaml") || strings.HasSuffix(d.Name(), ".yml")) {
			manifestFiles = append(manifestFiles, path)
		}

		return nil
	})
	if walkErr != nil {
		logger.Error(walkErr, "Failed to walk dist directory during deletion")

		return ctrl.Result{}, walkErr
	}
	logger.Info("Found manifest files for deletion", "count", len(manifestFiles))

	var parsedResources []*unstructured.Unstructured
	for _, manifestFile := range manifestFiles {
		logger.Info("Processing manifest file for deletion", "file", manifestFile)
		fileContent, readErr := os.ReadFile(manifestFile)
		if readErr != nil {
			logger.Error(readErr, "Failed to read manifest file during deletion", "file", manifestFile)

			return ctrl.Result{}, readErr
		}
		yamlDecoder := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(fileContent), 100)
		for {
			var rawObj runtime.RawExtension
			if err := yamlDecoder.Decode(&rawObj); err != nil {
				if err.Error() == "EOF" { // Check for clean EOF
					break
				}
				logger.Error(err, "Failed to decode YAML from manifest file during deletion", "file", manifestFile)

				return ctrl.Result{}, err // If not EOF, it's a real error
			}
			if rawObj.Raw == nil { // Should not happen with a successful decode, but good practice
				continue
			}
			u := &unstructured.Unstructured{}
			if _, _, err := unstructured.UnstructuredJSONScheme.Decode(rawObj.Raw, nil, u); err != nil {
				logger.Error(err, "Failed to decode RawExtension to Unstructured during deletion", "file", manifestFile)

				return ctrl.Result{}, err
			}
			parsedResources = append(parsedResources, u)
			logger.Info("Parsed resource for deletion", "GVK", u.GroupVersionKind().String(), "Name", u.GetName(), "Namespace", u.GetNamespace())
		}
	}
	logger.Info("Total resources parsed for deletion", "count", len(parsedResources))

	clusterList := &clusterv1.ClusterList{}
	selector, err := metav1.LabelSelectorAsSelector(&cdk8sAppProxy.Spec.ClusterSelector)
	if err != nil {
		logger.Error(err, "failed to parse ClusterSelector during deletion")

		return ctrl.Result{}, err
	}

	logger.Info("Listing clusters for deletion", "selector", selector.String(), "namespace", cdk8sAppProxy.Namespace)
	if err := r.List(ctx, clusterList, client.MatchingLabelsSelector{Selector: selector}, client.InNamespace(cdk8sAppProxy.Namespace)); err != nil {
		logger.Error(err, "Failed to list clusters during deletion, requeuing")

		return ctrl.Result{}, err // Requeue as we need to process clusters
	}
	clusterNamesDelete := make([]string, 0, len(clusterList.Items))
	for _, c := range clusterList.Items {
		clusterNamesDelete = append(clusterNamesDelete, c.Name)
	}
	logger.Info("Found clusters for deletion", "count", len(clusterList.Items), "names", clusterNamesDelete)

	for _, cluster := range clusterList.Items {
		logger := logger.WithValues("targetCluster", cluster.Name)
		logger.Info("Deleting resources from cluster")
		dynamicClient, err := r.getDynamicClientForCluster(ctx, cdk8sAppProxy.Namespace, cluster.Name)
		if err != nil {
			logger.Error(err, "Failed to get dynamic client for cluster during deletion, skipping this cluster")

			continue
		}
		logger.Info("Successfully created dynamic client for cluster deletion")
		for _, resource := range parsedResources {
			gvr := resource.GroupVersionKind().GroupVersion().WithResource(getPluralFromKind(resource.GetKind()))
			logger.Info("Deleting resource from cluster", "GVK", resource.GroupVersionKind().String(), "Name", resource.GetName(), "Namespace", resource.GetNamespace())
			err := dynamicClient.Resource(gvr).Namespace(resource.GetNamespace()).Delete(ctx, resource.GetName(), metav1.DeleteOptions{})
			if err != nil && !apierrors.IsNotFound(err) {
				logger.Error(err, "Failed to delete resource from cluster", "resourceName", resource.GetName())
			} else if apierrors.IsNotFound(err) {
				logger.Info("Resource already deleted from cluster", "resourceName", resource.GetName())
			} else {
				logger.Info("Successfully deleted resource from cluster", "resourceName", resource.GetName())
			}
		}
	}

	// Before removing the finalizer, cancel any active watches for this Cdk8sAppProxy.
	proxyNamespacedName = types.NamespacedName{Name: cdk8sAppProxy.Name, Namespace: cdk8sAppProxy.Namespace}
	if watchesForProxy, ok := r.ActiveWatches[proxyNamespacedName]; ok {
		logger.Info("Cancelling active watches for Cdk8sAppProxy before deletion", "count", len(watchesForProxy))
		for watchKey, cancelFunc := range watchesForProxy {
			logger.Info("Cancelling watch", "watchKey", watchKey)
			cancelFunc() // Stop the goroutine and its associated Kubernetes watch
		}
		// After all watches for this proxy are cancelled, remove its entry from the main map.
		delete(r.ActiveWatches, proxyNamespacedName)
		logger.Info("Removed Cdk8sAppProxy entry from ActiveWatches map")
	} else {
		logger.Info("No active watches found for this Cdk8sAppProxy to cancel.")
	}

	logger.Info("Finished deletion logic, removing finalizer")
	controllerutil.RemoveFinalizer(cdk8sAppProxy, Cdk8sAppProxyFinalizer)
	if err := r.Update(ctx, cdk8sAppProxy); err != nil {
		logger.Error(err, "Failed to remove finalizer")

		return ctrl.Result{}, err
	}
	logger.Info("Finalizer removed successfully")

	return ctrl.Result{}, nil
}

func (r *Cdk8sAppProxyReconciler) getDynamicClientForCluster(ctx context.Context, secretNamespace, clusterName string) (dynamic.Interface, error) {
	logger := log.FromContext(ctx).WithValues("secretNamespace", secretNamespace, "clusterName", clusterName)
	kubeconfigSecretName := clusterName + "-kubeconfig"
	logger.Info("Attempting to get Kubeconfig secret", "secretName", kubeconfigSecretName)
	kubeconfigSecret := &k8scorev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: secretNamespace, Name: kubeconfigSecretName}, kubeconfigSecret); err != nil {
		logger.Error(err, "Failed to get Kubeconfig secret")

		return nil, errors.Wrapf(err, "failed to get kubeconfig secret %s/%s", secretNamespace, kubeconfigSecretName)
	}
	kubeconfigData, ok := kubeconfigSecret.Data["value"]
	if !ok || len(kubeconfigData) == 0 {
		newErr := errors.Errorf("kubeconfig secret %s/%s does not contain 'value' data", secretNamespace, kubeconfigSecretName)
		logger.Error(newErr, "Invalid Kubeconfig secret")

		return nil, newErr
	}
	logger.Info("Successfully retrieved Kubeconfig data")
	restConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeconfigData)
	if err != nil {
		logger.Error(err, "Failed to create REST config from Kubeconfig")

		return nil, errors.Wrapf(err, "failed to create REST config from kubeconfig for cluster %s", clusterName)
	}
	logger.Info("Successfully created REST config")
	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		logger.Error(err, "Failed to create dynamic client")

		return nil, errors.Wrapf(err, "failed to create dynamic client for cluster %s", clusterName)
	}
	logger.Info("Successfully created dynamic client")

	return dynamicClient, nil
}

// func (r *Cdk8sAppProxyReconciler) getDynamicClientForCluster(ctx context.Context, proxyNamespace, clusterName string) (dynamic.Interface, error) {
// 	logger := log.FromContext(ctx).WithValues("proxyNamespace", proxyNamespace, "clusterName", clusterName)
// 	kubeconfigSecretName := clusterName + "-kubeconfig"
// 	logger.Info("Attempting to get Kubeconfig secret", "secretName", kubeconfigSecretName)
// 	kubeconfigSecret := &corev1.Secret{}
// 	if err := r.Get(ctx, client.ObjectKey{Namespace: proxyNamespace, Name: kubeconfigSecretName}, kubeconfigSecret); err != nil {
// 		logger.Error(err, "Failed to get Kubeconfig secret")
// 		return nil, errors.Wrapf(err, "failed to get kubeconfig secret %s/%s", proxyNamespace, kubeconfigSecretName)
// 	}
// 	kubeconfigData, ok := kubeconfigSecret.Data["value"]
// 	if !ok || len(kubeconfigData) == 0 {
// 		newErr := errors.Errorf("kubeconfig secret %s/%s does not contain 'value' data", proxyNamespace, kubeconfigSecretName)
// 		logger.Error(newErr, "Invalid Kubeconfig secret")

// 		return nil, newErr
// 	}
// 	logger.Info("Successfully retrieved Kubeconfig data")
// 	restConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeconfigData)
// 	if err != nil {
// 		logger.Error(err, "Failed to create REST config from Kubeconfig")
// 		return nil, errors.Wrapf(err, "failed to create REST config from kubeconfig for cluster %s", clusterName)
// 	}
// 	logger.Info("Successfully created REST config")
// 	dynamicClient, err := dynamic.NewForConfig(restConfig)
// 	if err != nil {
// 		logger.Error(err, "Failed to create dynamic client")

// 		return nil, errors.Wrapf(err, "failed to create dynamic client for cluster %s", clusterName)
// 	}
// 	logger.Info("Successfully created dynamic client")

// 	return dynamicClient, nil
// }

// handleError is a helper to consistently update status conditions and log errors.
func (r *Cdk8sAppProxyReconciler) handleError(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, reason, messageFormat string, err error) error {
	logger := log.FromContext(ctx).WithValues("cdk8sappproxy", types.NamespacedName{Name: cdk8sAppProxy.Name, Namespace: cdk8sAppProxy.Namespace})
	logger.Error(err, "Reconciliation error", "reason", reason, "messageFormat", messageFormat)
	conditions.MarkFalse(cdk8sAppProxy, addonsv1alpha1.DeploymentProgressingCondition, reason, clusterv1.ConditionSeverityError, messageFormat, err.Error()) // Pass err.Error() for message
	if statusUpdateErr := r.Status().Update(ctx, cdk8sAppProxy); statusUpdateErr != nil {
		logger.Error(statusUpdateErr, "Failed to update status after error", "originalError", err.Error())
	}

	return err
}

// handleDeleteError is a helper for reconcileDelete to remove finalizer if non-requeueable error occurs.
func (r *Cdk8sAppProxyReconciler) handleDeleteError(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, message string, originalErr error) error {
	logger := log.FromContext(ctx).WithValues("cdk8sappproxy", types.NamespacedName{Name: cdk8sAppProxy.Name, Namespace: cdk8sAppProxy.Namespace})
	if originalErr != nil {
		logger.Error(originalErr, message+", proceeding to remove finalizer")
	} else {
		logger.Info(message + ", proceeding to remove finalizer")
	}
	controllerutil.RemoveFinalizer(cdk8sAppProxy, Cdk8sAppProxyFinalizer)
	if err := r.Update(ctx, cdk8sAppProxy); err != nil {
		logger.Error(err, "Failed to remove finalizer after error on delete")

		return err
	}
	logger.Info("Removed finalizer after error/condition on delete")
	if originalErr != nil {
		return originalErr // Return original error to potentially requeue if it's something transient related to the object update itself
	}

	return nil // Successfully handled by removing finalizer
}

func truncateString(str string, num int) string {
	if len(str) > num {
		return str[0:num] + "..."
	}

	return str
}

// TODO: This is a naive pluralization and might not work for all kinds.
// A more robust solution would use discovery client or a predefined map.
func getPluralFromKind(kind string) string {
	lowerKind := strings.ToLower(kind)
	if strings.HasSuffix(lowerKind, "s") {
		return lowerKind + "es"
	}
	if strings.HasSuffix(lowerKind, "y") {
		return strings.TrimSuffix(lowerKind, "y") + "ies"
	}

	return lowerKind + "s"
}

// SetupWithManager sets up the controller with the Manager.
func (r *Cdk8sAppProxyReconciler) SetupWithManager(mgr ctrl.Manager, options controller.Options) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&addonsv1alpha1.Cdk8sAppProxy{}).
		WithOptions(options). // Add this line
		Watches(
			&clusterv1.Cluster{},
			handler.EnqueueRequestsFromMapFunc(r.ClusterToCdk8sAppProxyMapper),
		).
		Complete(r)
}

// ClusterToCdk8sAppProxyMapper is a handler.ToRequestsFunc to be used to enqeue requests for Cdk8sAppProxyReconciler.
// It maps CAPI Cluster events to Cdk8sAppProxy events.
func (r *Cdk8sAppProxyReconciler) ClusterToCdk8sAppProxyMapper(ctx context.Context, o client.Object) []ctrl.Request {
	logger := log.FromContext(ctx)
	cluster, ok := o.(*clusterv1.Cluster)
	if !ok {
		logger.Error(errors.Errorf("unexpected type %T, expected Cluster", o), "failed to cast object to Cluster", "object", o)

		return nil
	}

	logger = logger.WithValues("clusterName", cluster.Name, "clusterNamespace", cluster.Namespace)
	logger.Info("ClusterToCdk8sAppProxyMapper triggered for cluster")

	proxies := &addonsv1alpha1.Cdk8sAppProxyList{}
	// List all Cdk8sAppProxies in the same namespace as the Cluster.
	// Adjust if Cdk8sAppProxy can be in a different namespace or cluster-scoped.
	// For now, assuming Cdk8sAppProxy is namespace-scoped and in the same namespace as the triggering Cluster's Cdk8sAppProxy object (which is usually the management cluster's default namespace).
	// However, Cdk8sAppProxy resources themselves select clusters across namespaces.
	// So, we should list Cdk8sAppProxies from all namespaces if the controller has cluster-wide watch permissions for them.
	// If the controller is namespace-scoped for Cdk8sAppProxy, this list will be limited.
	// For this example, let's assume cluster-wide list for Cdk8sAppProxy.
	if err := r.List(ctx, proxies); err != nil { // staticcheck: QF1008
		logger.Error(err, "failed to list Cdk8sAppProxies")

		return nil
	}
	logger.Info("Checking Cdk8sAppProxies for matches", "count", len(proxies.Items))

	var requests []ctrl.Request
	for _, proxy := range proxies.Items {
		proxyLogger := logger.WithValues("cdk8sAppProxyName", proxy.Name, "cdk8sAppProxyNamespace", proxy.Namespace)
		proxyLogger.Info("Evaluating Cdk8sAppProxy")

		selector, err := metav1.LabelSelectorAsSelector(&proxy.Spec.ClusterSelector)
		if err != nil {
			proxyLogger.Error(err, "failed to parse ClusterSelector for Cdk8sAppProxy")

			continue
		}
		proxyLogger.Info("Parsed ClusterSelector", "selector", selector.String())

		if selector.Matches(labels.Set(cluster.GetLabels())) {
			proxyLogger.Info("Cluster labels match Cdk8sAppProxy selector, enqueuing request")
			requests = append(requests, ctrl.Request{
				NamespacedName: client.ObjectKey{
					Namespace: proxy.Namespace,
					Name:      proxy.Name,
				},
			})
		} else {
			proxyLogger.Info("Cluster labels do not match Cdk8sAppProxy selector")
		}
	}

	logger.Info("ClusterToCdk8sAppProxyMapper finished", "requestsEnqueued", len(requests))

	return requests
}

func (r *Cdk8sAppProxyReconciler) watchResourceOnTargetCluster(
	ctx context.Context, // This context is the one that can be cancelled to stop the watch
	targetClient dynamic.Interface,
	gvk schema.GroupVersionKind,
	namespace string,
	name string,
	parentProxy types.NamespacedName,
	watchKey string, // For logging and map management
) {
	logger := log.FromContext(ctx).WithValues(
		"watchKey", watchKey,
		"gvk", gvk.String(),
		"resourceNamespace", namespace,
		"resourceName", name,
		"parentProxy", parentProxy.String(),
	)
	logger.Info("Starting watch for resource on target cluster")

	gvr := gvk.GroupVersion().WithResource(getPluralFromKind(gvk.Kind))

	watcher, err := targetClient.Resource(gvr).Namespace(namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector: "metadata.name=" + name,
		// Watch:         true, // Watch is implicit in Watch() method
	})
	if err != nil {
		logger.Error(err, "Failed to start watch for resource")
		// Clean up the watch from the map if it failed to start
		if r.ActiveWatches[parentProxy] != nil {
			if cancelFn, ok := r.ActiveWatches[parentProxy][watchKey]; ok {
				cancelFn() // Call cancel to ensure context is cleaned up if partially initialized
				delete(r.ActiveWatches[parentProxy], watchKey)
			}
		}
		return
	}
	defer watcher.Stop() // Ensure watcher is stopped when goroutine exits

	for {
		select {
		case event, ok := <-watcher.ResultChan():
			if !ok {
				logger.Info("Watch channel closed for resource. Parent controller will re-trigger reconciliation if needed.")
				// Remove from activeWatches as this specific watch instance is now defunct.
				// The parent reconcile loop will re-establish if the resource still needs to be watched.
				if r.ActiveWatches[parentProxy] != nil {
					// Check if the current cancel func is still the one we started with,
					// though simple deletion is usually fine as reconcileNormal will overwrite.
					delete(r.ActiveWatches[parentProxy], watchKey)
				}
				return // Exit goroutine
			}

			logger.Info("Received watch event", "type", event.Type) // Avoid logging event.Object directly in production if it's large

			if event.Type == watch.Deleted {
				logger.Info("Resource deletion detected, triggering re-reconciliation of parent proxy", "parentProxy", parentProxy.String())

				// Create a new context for fetching and updating the parent proxy object.
				updateCtx := context.Background()
				parentProxyObj := &addonsv1alpha1.Cdk8sAppProxy{}

				// Fetch the Cdk8sAppProxy object.
				if err := r.Client.Get(updateCtx, parentProxy, parentProxyObj); err != nil {
					logger.Error(err, "Failed to get parent Cdk8sAppProxy for re-reconciliation", "parentProxy", parentProxy.String())
				} else {
					// Initialize annotations if nil.
					if parentProxyObj.Annotations == nil {
						parentProxyObj.Annotations = make(map[string]string)
					}
					// Set an annotation to trigger reconciliation.
					parentProxyObj.Annotations["cdk8s.addons.cluster.x-k8s.io/reconcile-on-delete-trigger"] = metav1.Now().Format(time.RFC3339Nano)

					// Update the Cdk8sAppProxy object.
					if err := r.Client.Update(updateCtx, parentProxyObj); err != nil {
						logger.Error(err, "Failed to update parent Cdk8sAppProxy for re-reconciliation", "parentProxy", parentProxy.String())
					} else {
						logger.Info("Successfully updated parent Cdk8sAppProxy to trigger re-reconciliation", "parentProxy", parentProxy.String())
					}
				}

				logger.Info("Resource deleted on target cluster. Cancelling this watch and removing from active watches. Parent Cdk8sAppProxy will re-reconcile.")
				// The resource is deleted. This watch is no longer valid.
				// Cancel this watch's context (which calls defer watcher.Stop())
				// and remove it from the activeWatches map.
				// The main reconcile loop of the parent Cdk8sAppProxy will eventually run
				// (due to resync period or other events) and will attempt to re-create
				// the resource and a new watch for it.
				if r.ActiveWatches[parentProxy] != nil {
					if cancelFn, ok := r.ActiveWatches[parentProxy][watchKey]; ok {
						cancelFn() // This will cause the ctx.Done() case to be selected if not already.
						delete(r.ActiveWatches[parentProxy], watchKey)
					}
				}
				// It's important to return here as this specific watch instance is done.
				return
			}
			// For other events (Added, Modified), we just log and continue watching.
			// The main reconciliation loop is responsible for desired state enforcement.
			// This watch is primarily to detect deletions and clean up itself.

		case <-ctx.Done():
			logger.Info("Watch context cancelled for resource. Stopping watch.")
			// Context was cancelled (likely by reconcileNormal stopping this watch, or reconciler shutting down).
			// Remove this watch from the activeWatches map.
			if r.ActiveWatches[parentProxy] != nil {
				delete(r.ActiveWatches[parentProxy], watchKey)
			}
			return // Exit goroutine
		}
	}
}
