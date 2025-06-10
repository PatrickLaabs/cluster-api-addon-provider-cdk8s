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

package cdk8sappproxy

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
	corev1 "k8s.io/api/core/v1"
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
			if err := r.pollGitRepositoryOnce(ctx, proxyName, logger); err != nil {
				logger.Error(err, "Error during git repository polling")
			}
		case <-ctx.Done():
			logger.Info("Stopping git repository polling loop due to context cancellation.")

			return
		}
	}
}

func (r *Cdk8sAppProxyReconciler) pollGitRepositoryOnce(ctx context.Context, proxyName types.NamespacedName, logger logr.Logger) error {
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

func (r *Cdk8sAppProxyReconciler) getCdk8sAppProxyForPolling(ctx context.Context, proxyName types.NamespacedName) (*addonsv1alpha1.Cdk8sAppProxy, error) {
	cdk8sAppProxy := &addonsv1alpha1.Cdk8sAppProxy{}
	if err := r.Get(ctx, proxyName, cdk8sAppProxy); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}

		return nil, err
	}

	return cdk8sAppProxy, nil
}

func (r *Cdk8sAppProxyReconciler) isGitRepositoryConfigured(cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, logger logr.Logger) bool {
	if cdk8sAppProxy.Spec.GitRepository == nil || cdk8sAppProxy.Spec.GitRepository.URL == "" {
		logger.Info("GitRepository not configured for this Cdk8sAppProxy, stopping polling.")

		return false
	}

	return true
}

func (r *Cdk8sAppProxyReconciler) determineGitReference(gitSpec *addonsv1alpha1.GitRepositorySpec, logger logr.Logger) plumbing.ReferenceName {
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

func (r *Cdk8sAppProxyReconciler) fetchRemoteCommitHash(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, gitSpec *addonsv1alpha1.GitRepositorySpec, refName plumbing.ReferenceName, logger logr.Logger) (string, error) {
	logger.Info("Attempting to LsRemote", "url", gitSpec.URL, "refName", refName.String())

	rem := git.NewRemote(memory.NewStorage(), &config.RemoteConfig{
		URLs: []string{gitSpec.URL},
	})

	auth, err := r.getGitAuthForPolling(ctx, cdk8sAppProxy, gitSpec, logger)
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

func (r *Cdk8sAppProxyReconciler) getGitAuthForPolling(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, gitSpec *addonsv1alpha1.GitRepositorySpec, logger logr.Logger) (*http.BasicAuth, error) {
	auth := &http.BasicAuth{}
	if gitSpec.AuthSecretRef == nil {
		return auth, nil
	}

	secret := &corev1.Secret{}
	secretKey := client.ObjectKey{Namespace: cdk8sAppProxy.Namespace, Name: gitSpec.AuthSecretRef.Name}
	if err := r.Get(ctx, secretKey, secret); err != nil {
		logger.Error(err, "Failed to get auth secret for LsRemote")

		return nil, err
	}

	username, userOk := secret.Data["username"]
	password, passOk := secret.Data["password"]
	if !userOk || !passOk {
		err := errors.New("auth secret missing username or password")
		logger.Error(err, "secretName", secretKey.String())

		return nil, err
	}

	auth.Username = string(username)
	auth.Password = string(password)
	logger.Info("Using credentials from AuthSecretRef for LsRemote")

	return auth, nil
}

func (r *Cdk8sAppProxyReconciler) findRemoteCommitHash(refs []*plumbing.Reference, refName plumbing.ReferenceName, logger logr.Logger) (string, error) {
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

func (r *Cdk8sAppProxyReconciler) tryFindDefaultBranch(refs []*plumbing.Reference, refName plumbing.ReferenceName, logger logr.Logger) (bool, string) {
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

func (r *Cdk8sAppProxyReconciler) handleGitRepositoryChange(ctx context.Context, proxyName types.NamespacedName, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, remoteCommitHash string, logger logr.Logger) error {
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

func (r *Cdk8sAppProxyReconciler) updateRemoteGitHashStatus(ctx context.Context, proxyName types.NamespacedName, remoteCommitHash string, logger logr.Logger) error {
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

func (r *Cdk8sAppProxyReconciler) triggerReconciliation(ctx context.Context, proxyName types.NamespacedName, logger logr.Logger) error {
	proxyToAnnotate := &addonsv1alpha1.Cdk8sAppProxy{}
	if err := r.Get(ctx, proxyName, proxyToAnnotate); err != nil {
		logger.Error(err, "Failed to get latest Cdk8sAppProxy for annotation update")

		return err
	}

	if proxyToAnnotate.Annotations == nil {
		proxyToAnnotate.Annotations = make(map[string]string)
	}
	proxyToAnnotate.Annotations["cdk8s.addons.cluster.x-k8s.io/git-poll-trigger"] = time.Now().Format(time.RFC3339Nano)

	if err := r.Update(ctx, proxyToAnnotate); err != nil {
		logger.Error(err, "Failed to update Cdk8sAppProxy annotations to trigger reconciliation")

		return err
	}

	logger.Info("Successfully updated annotations to trigger reconciliation.")

	return nil
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

// checkIfResourceExists checks if a given resource exists on the target cluster. It uses a dynamic client to make the check.
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
				return false, nil
			}
			// Some other error occurred
			return false, errors.Wrapf(err, "failed to get cluster-scoped resource %s with GVR %s", name, gvr.String())
		}
	}

	return true, nil
}

func (r *Cdk8sAppProxyReconciler) stopGitPoller(proxyNamespacedName types.NamespacedName, logger logr.Logger) {
	if cancel, ok := r.activeGitPollers[proxyNamespacedName]; ok {
		logger.Info("Stopping git poller due to resource deletion.")
		cancel()
		delete(r.activeGitPollers, proxyNamespacedName)
	}
}

func (r *Cdk8sAppProxyReconciler) prepareSourceForDeletion(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, logger logr.Logger) (string, func(), error) {
	if cdk8sAppProxy.Spec.GitRepository != nil && cdk8sAppProxy.Spec.GitRepository.URL != "" {
		return r.prepareGitSourceForDeletion(ctx, cdk8sAppProxy, logger)
	} else if cdk8sAppProxy.Spec.LocalPath != "" {
		return cdk8sAppProxy.Spec.LocalPath, func() {}, nil
	}

	err := errors.New("neither GitRepository nor LocalPath specified, cannot determine resources to delete")
	logger.Info(err.Error())
	_ = r.handleDeleteError(ctx, cdk8sAppProxy, err.Error(), nil)

	return "", func() {}, err
}

func (r *Cdk8sAppProxyReconciler) prepareGitSourceForDeletion(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, logger logr.Logger) (string, func(), error) {
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

	cloneOptions := &git.CloneOptions{
		URL:      gitSpec.URL,
		Progress: &gitProgressLogger{logger: logger.WithName("git-clone-delete")},
	}

	// Handle authentication if specified
	if gitSpec.AuthSecretRef != nil {
		auth, err := r.getGitAuth(ctx, cdk8sAppProxy, gitSpec.AuthSecretRef, logger)
		if err != nil {
			cleanupFunc()

			return "", func() {}, err
		}
		cloneOptions.Auth = auth
	}

	// Clone repository
	if err := r.cloneGitRepoForDeletion(ctx, tempDir, cloneOptions, logger); err != nil {
		cleanupFunc()

		return "", func() {}, err
	}

	// Checkout specific reference if specified
	if gitSpec.Reference != "" {
		if err := r.checkoutGitReferenceForDeletion(tempDir, gitSpec.Reference, logger); err != nil {
			cleanupFunc()

			return "", func() {}, err
		}
	}

	appSourcePath := tempDir
	if gitSpec.Path != "" {
		appSourcePath = filepath.Join(tempDir, gitSpec.Path)
		logger.Info("Adjusted appSourcePath for deletion", "subPath", gitSpec.Path, "finalPath", appSourcePath)
	}

	return appSourcePath, cleanupFunc, nil
}

func (r *Cdk8sAppProxyReconciler) getGitAuth(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, authSecretRef *corev1.LocalObjectReference, logger logr.Logger) (*http.BasicAuth, error) {
	logger.Info("AuthSecretRef specified for deletion clone, attempting to fetch secret", "secretName", authSecretRef.Name)

	authSecret := &corev1.Secret{}
	secretKey := client.ObjectKey{Namespace: cdk8sAppProxy.Namespace, Name: authSecretRef.Name}
	if err := r.Get(ctx, secretKey, authSecret); err != nil {
		logger.Error(err, "Failed to get auth secret for git clone during deletion", "secretName", secretKey.String())

		return nil, err
	}

	username, okUser := authSecret.Data["username"]
	password, okPass := authSecret.Data["password"]

	if !okUser || !okPass {
		err := errors.New("auth secret missing username or password fields for deletion clone")
		logger.Error(err, "Invalid auth secret for deletion clone", "secretName", secretKey.String())

		return nil, err
	}

	logger.Info("Successfully fetched auth secret for deletion clone")

	return &http.BasicAuth{
		Username: string(username),
		Password: string(password),
	}, nil
}

func (r *Cdk8sAppProxyReconciler) cloneGitRepoForDeletion(ctx context.Context, tempDir string, cloneOptions *git.CloneOptions, logger logr.Logger) error {
	logger.Info("Executing git clone with go-git for deletion", "url", cloneOptions.URL, "targetDir", tempDir)

	_, err := git.PlainCloneContext(ctx, tempDir, false, cloneOptions)
	if err != nil {
		logger.Error(err, "go-git PlainCloneContext failed during deletion")
		reason := addonsv1alpha1.GitCloneFailedReason
		if errors.Is(err, plumbingtransport.ErrAuthenticationRequired) || strings.Contains(err.Error(), "authentication required") {
			reason = addonsv1alpha1.GitAuthenticationFailedReason
		}
		logger.Error(err, "go-git clone failed during deletion", "reason", reason)

		return err
	}

	logger.Info("Successfully cloned git repository with go-git for deletion")

	return nil
}

func (r *Cdk8sAppProxyReconciler) checkoutGitReferenceForDeletion(tempDir, reference string, logger logr.Logger) error {
	logger.Info("Executing git checkout with go-git for deletion", "reference", reference, "dir", tempDir)

	repo, err := git.PlainOpen(tempDir)
	if err != nil {
		logger.Error(err, "go-git PlainOpen failed during deletion")

		return err
	}

	worktree, err := repo.Worktree()
	if err != nil {
		logger.Error(err, "go-git Worktree failed during deletion")

		return err
	}

	checkoutOpts := &git.CheckoutOptions{Force: true}
	if plumbing.IsHash(reference) {
		checkoutOpts.Hash = plumbing.NewHash(reference)
	} else {
		revision := plumbing.Revision(reference)
		resolvedHash, err := repo.ResolveRevision(revision)
		if err != nil {
			logger.Error(err, "go-git ResolveRevision failed during deletion", "reference", reference)

			return err
		}
		checkoutOpts.Hash = *resolvedHash
	}

	err = worktree.Checkout(checkoutOpts)
	if err != nil {
		logger.Error(err, "go-git Checkout failed during deletion", "reference", reference)

		return err
	}

	logger.Info("Successfully checked out git reference with go-git for deletion", "reference", reference)

	return nil
}

func (r *Cdk8sAppProxyReconciler) synthesizeAndParseResources(appSourcePath string, logger logr.Logger) ([]*unstructured.Unstructured, error) {
	// Synthesize cdk8s application
	if err := r.synthesizeCdk8sApp(appSourcePath, logger); err != nil {
		return nil, err
	}

	// Find manifest files
	manifestFiles, err := r.findManifestFiles(appSourcePath, logger)
	if err != nil {
		return nil, err
	}

	// Parse resources from manifest files
	return r.parseResourcesFromManifests(manifestFiles, logger)
}

func (r *Cdk8sAppProxyReconciler) synthesizeCdk8sApp(appSourcePath string, logger logr.Logger) error {
	logger.Info("Synthesizing cdk8s application to identify resources for deletion", "effectiveSourcePath", appSourcePath)

	synthCmd := cmdRunnerFactory("cdk8s", "synth")
	synthCmd.SetDir(appSourcePath)
	output, synthErr := synthCmd.CombinedOutput()
	if synthErr != nil {
		logger.Error(synthErr, "cdk8s synth failed during deletion", "output", string(output))

		return synthErr
	}

	logger.Info("cdk8s synth successful for deletion", "outputSummary", truncateString(string(output), 200))
	logger.V(1).Info("cdk8s synth full output for deletion", "output", string(output))

	return nil
}

func (r *Cdk8sAppProxyReconciler) findManifestFiles(appSourcePath string, logger logr.Logger) ([]string, error) {
	distPath := filepath.Join(appSourcePath, "dist")
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

		return nil, walkErr
	}

	logger.Info("Found manifest files for deletion", "count", len(manifestFiles))

	return manifestFiles, nil
}

func (r *Cdk8sAppProxyReconciler) parseResourcesFromManifests(manifestFiles []string, logger logr.Logger) ([]*unstructured.Unstructured, error) {
	var parsedResources []*unstructured.Unstructured

	for _, manifestFile := range manifestFiles {
		resources, err := r.parseResourcesFromSingleManifest(manifestFile, logger)
		if err != nil {
			return nil, err
		}
		parsedResources = append(parsedResources, resources...)
	}

	logger.Info("Total resources parsed for deletion", "count", len(parsedResources))

	return parsedResources, nil
}

func (r *Cdk8sAppProxyReconciler) parseResourcesFromSingleManifest(manifestFile string, logger logr.Logger) ([]*unstructured.Unstructured, error) {
	logger.Info("Processing manifest file for deletion", "file", manifestFile)

	fileContent, readErr := os.ReadFile(manifestFile)
	if readErr != nil {
		logger.Error(readErr, "Failed to read manifest file during deletion", "file", manifestFile)

		return nil, readErr
	}

	var parsedResources []*unstructured.Unstructured
	yamlDecoder := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(fileContent), 100)

	for {
		var rawObj runtime.RawExtension
		if err := yamlDecoder.Decode(&rawObj); err != nil {
			if err.Error() == "EOF" {
				break
			}
			logger.Error(err, "Failed to decode YAML from manifest file during deletion", "file", manifestFile)

			return nil, err
		}

		if rawObj.Raw == nil {
			continue
		}

		u := &unstructured.Unstructured{}
		if _, _, err := unstructured.UnstructuredJSONScheme.Decode(rawObj.Raw, nil, u); err != nil {
			logger.Error(err, "Failed to decode RawExtension to Unstructured during deletion", "file", manifestFile)

			return nil, err
		}

		parsedResources = append(parsedResources, u)
		logger.Info("Parsed resource for deletion", "GVK", u.GroupVersionKind().String(), "Name", u.GetName(), "Namespace", u.GetNamespace())
	}

	return parsedResources, nil
}

func (r *Cdk8sAppProxyReconciler) deleteResourcesFromClusters(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, parsedResources []*unstructured.Unstructured, logger logr.Logger) error {
	// Get target clusters
	clusterList, err := r.getTargetClustersForDeletion(ctx, cdk8sAppProxy, logger)
	if err != nil {
		return err
	}

	// Delete resources from each cluster
	for _, cluster := range clusterList.Items {
		if err := r.deleteResourcesFromSingleCluster(ctx, cdk8sAppProxy, cluster, parsedResources, logger); err != nil {
			// Log error but continue with other clusters
			logger.Error(err, "Failed to delete resources from cluster", "cluster", cluster.Name)
		}
	}

	return nil
}

func (r *Cdk8sAppProxyReconciler) getTargetClustersForDeletion(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, logger logr.Logger) (*clusterv1.ClusterList, error) {
	clusterList := &clusterv1.ClusterList{}
	selector, err := metav1.LabelSelectorAsSelector(&cdk8sAppProxy.Spec.ClusterSelector)
	if err != nil {
		logger.Error(err, "failed to parse ClusterSelector during deletion")

		return nil, err
	}

	logger.Info("Listing clusters for deletion", "selector", selector.String(), "namespace", cdk8sAppProxy.Namespace)
	if err := r.List(ctx, clusterList, client.MatchingLabelsSelector{Selector: selector}, client.InNamespace(cdk8sAppProxy.Namespace)); err != nil {
		logger.Error(err, "Failed to list clusters during deletion, requeuing")

		return nil, err
	}

	clusterNames := make([]string, 0, len(clusterList.Items))
	for _, c := range clusterList.Items {
		clusterNames = append(clusterNames, c.Name)
	}
	logger.Info("Found clusters for deletion", "count", len(clusterList.Items), "names", clusterNames)

	return clusterList, nil
}

func (r *Cdk8sAppProxyReconciler) deleteResourcesFromSingleCluster(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, cluster clusterv1.Cluster, parsedResources []*unstructured.Unstructured, logger logr.Logger) error {
	clusterLogger := logger.WithValues("targetCluster", cluster.Name)
	clusterLogger.Info("Deleting resources from cluster")

	dynamicClient, err := r.getDynamicClientForCluster(ctx, cdk8sAppProxy.Namespace, cluster.Name)
	if err != nil {
		clusterLogger.Error(err, "Failed to get dynamic client for cluster during deletion, skipping this cluster")

		return err
	}

	clusterLogger.Info("Successfully created dynamic client for cluster deletion")

	for _, resource := range parsedResources {
		if err := r.deleteResourceFromCluster(ctx, dynamicClient, resource, clusterLogger); err != nil {
			// Log but continue with other resources
			clusterLogger.Error(err, "Failed to delete resource from cluster", "resourceName", resource.GetName())
		}
	}

	return nil
}

func (r *Cdk8sAppProxyReconciler) deleteResourceFromCluster(ctx context.Context, dynamicClient dynamic.Interface, resource *unstructured.Unstructured, logger logr.Logger) error {
	gvr := resource.GroupVersionKind().GroupVersion().WithResource(getPluralFromKind(resource.GetKind()))
	logger.Info("Deleting resource from cluster", "GVK", resource.GroupVersionKind().String(), "Name", resource.GetName(), "Namespace", resource.GetNamespace())

	err := dynamicClient.Resource(gvr).Namespace(resource.GetNamespace()).Delete(ctx, resource.GetName(), metav1.DeleteOptions{})

	switch {
	case err != nil && !apierrors.IsNotFound(err):
		logger.Error(err, "Failed to delete resource from cluster", "resourceName", resource.GetName())

		return err
	case apierrors.IsNotFound(err):
		logger.Info("Resource already deleted from cluster", "resourceName", resource.GetName())
	case err == nil:
		logger.Info("Successfully deleted resource from cluster", "resourceName", resource.GetName())
	}

	return nil
}

func (r *Cdk8sAppProxyReconciler) finalizeDeletion(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, proxyNamespacedName types.NamespacedName, logger logr.Logger) (ctrl.Result, error) {
	// Cancel any active watches for this Cdk8sAppProxy
	r.cancelActiveWatches(proxyNamespacedName, logger)

	// Remove finalizer
	return r.removeFinalizer(ctx, cdk8sAppProxy, logger)
}

func (r *Cdk8sAppProxyReconciler) cancelActiveWatches(proxyNamespacedName types.NamespacedName, logger logr.Logger) {
	if watchesForProxy, ok := r.ActiveWatches[proxyNamespacedName]; ok {
		logger.Info("Cancelling active watches for Cdk8sAppProxy before deletion", "count", len(watchesForProxy))
		for watchKey, cancelFunc := range watchesForProxy {
			logger.Info("Cancelling watch", "watchKey", watchKey)
			cancelFunc() // Stop the goroutine and its associated Kubernetes watch
		}
		// After all watches for this proxy are cancelled, remove its entry from the main map
		delete(r.ActiveWatches, proxyNamespacedName)
		logger.Info("Removed Cdk8sAppProxy entry from ActiveWatches map")
	} else {
		logger.Info("No active watches found for this Cdk8sAppProxy to cancel.")
	}
}

func (r *Cdk8sAppProxyReconciler) removeFinalizer(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, logger logr.Logger) (ctrl.Result, error) {
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
	kubeconfigSecret := &corev1.Secret{}
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
		return originalErr
	}

	return nil
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

				return
			}

			logger.Info("Received watch event", "type", event.Type) // Avoid logging event.Object directly in production if it's large

			if event.Type == watch.Deleted {
				logger.Info("Resource deletion detected, triggering re-reconciliation of parent proxy", "parentProxy", parentProxy.String())

				// Create a new context for fetching and updating the parent proxy object.
				updateCtx := ctx
				parentProxyObj := &addonsv1alpha1.Cdk8sAppProxy{}

				// Fetch the Cdk8sAppProxy object.
				if err := r.Get(updateCtx, parentProxy, parentProxyObj); err != nil {
					logger.Error(err, "Failed to get parent Cdk8sAppProxy for re-reconciliation", "parentProxy", parentProxy.String())
				} else {
					// Initialize annotations if nil.
					if parentProxyObj.Annotations == nil {
						parentProxyObj.Annotations = make(map[string]string)
					}
					// Set an annotation to trigger reconciliation.
					parentProxyObj.Annotations["cdk8s.addons.cluster.x-k8s.io/reconcile-on-delete-trigger"] = metav1.Now().Format(time.RFC3339Nano)

					// Update the Cdk8sAppProxy object.
					if err := r.Update(updateCtx, parentProxyObj); err != nil {
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

			return
		}
	}
}
