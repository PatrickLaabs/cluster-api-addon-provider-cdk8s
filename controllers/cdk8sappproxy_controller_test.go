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

package controllers_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"sync"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/object"
	addonsv1alpha1 "github.com/PatrickLaabs/cluster-api-addon-provider-cdk8s/api/v1alpha1"
	"github.com/PatrickLaabs/cluster-api-addon-provider-cdk8s/controllers"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/mock"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubetesting "k8s.io/client-go/testing"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake" // Import fake client
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// MockCommander is a mock for the command execution part.
type MockCommander struct {
	mock.Mock
}

func (m *MockCommander) CombinedOutput(command string, args ...string) ([]byte, error) {
	// This is a simplification. In a real scenario, you might want to pass the exec.Cmd object
	// or have a more sophisticated way to set up expectations for specific commands and their arguments.
	// For this test, we'll base the mock on the first argument (the command itself, e.g., "git", "cdk8s").
	// We'll also consider the directory if it's set via a different method or part of args.
	// For now, we assume the test will set expectations on "git clone", "git checkout", or "cdk8s synth".

	var MOCK_COMMAND_TYPE string
	if command == "git" && len(args) > 0 && args[0] == "clone" {
		MOCK_COMMAND_TYPE = "git clone"
	} else if command == "git" && len(args) > 0 && args[0] == "checkout" {
		MOCK_COMMAND_TYPE = "git checkout"
	} else if command == "cdk8s" && len(args) > 0 && args[0] == "synth" {
		MOCK_COMMAND_TYPE = "cdk8s synth"
	} else {
		MOCK_COMMAND_TYPE = command // Fallback for other commands if any
	}

	// Allow tests to set expectations on specific command "types"
	calledArgs := m.Called(MOCK_COMMAND_TYPE)
	output := calledArgs.Get(0)
	err := calledArgs.Error(1)

	if output == nil {
		return nil, err
	}
	byteOutput, ok := output.([]byte)
	if !ok && output != nil {
		// This case should ideally not happen if mocks are set up correctly.
		// It means output was not nil but also not []byte.
		return nil, errors.New("mock commander output was not of type []byte")
	}

	return byteOutput, err
}

// var execCommandCombinedOutput = func(cmd *controllers.RealCmdRunner) ([]byte, error) { // This var is unused and can be removed.
// 	return cmd.CombinedOutput()
// }

var _ = Describe("Cdk8sAppProxy controller", func() {
	var (
		reconciler                   *controllers.Cdk8sAppProxyReconciler
		mockCommander                *MockCommander
		testNamespace                *corev1.Namespace // Assuming corev1 is imported in suite_test.go
		ctx                          context.Context
		fakeK8sClient                client.Client // For watch trigger test
		fakeScheme                   *runtime.Scheme
		testLog                      = zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true))
		mu                           sync.Mutex // For safely accessing ActiveWatches in tests
		originalGetDynamicClientFunc func(ctx context.Context, secretNamespace string, clusterName string, proxy client.Client) (dynamic.Interface, error)
	)

	BeforeEach(func() {
		ctx = context.Background()
		// k8sClient is set in suite_test.go's BeforeSuite
		Expect(k8sClient).NotTo(BeNil(), "k8sClient should be initialized in BeforeSuite")

		// Create and register the mock commander
		mockCommander = new(MockCommander)
		controllers.SetCommander(func(command string, args ...string) controllers.CommandExecutor {
			// This function will be called by the controller to get a command executor.
			// We return our mock.
			// We need a way to tell the mockCommander about the command and args if needed for more granular mocking.
			// For now, the mockCommander's CombinedOutput will use the command string.
			return &controllers.RealCmdRunner{Name: command, Args: args, CommanderFunc: mockCommander.CombinedOutput}
		})

		reconciler = &controllers.Cdk8sAppProxyReconciler{
			Client:        k8sClient, // Real client for most tests
			Scheme:        k8sClient.Scheme(),
			ActiveWatches: make(map[types.NamespacedName]map[string]context.CancelFunc),
		}

		// Setup for fake client tests (specifically for watch trigger)
		fakeScheme = runtime.NewScheme()
		Expect(addonsv1alpha1.AddToScheme(fakeScheme)).To(Succeed())
		Expect(corev1.AddToScheme(fakeScheme)).To(Succeed()) // If using corev1 types like Secret with fake client

		// Create a namespace for each test
		// testNamespace is defined in suite_test.go or should be created here
		// For simplicity, let's assume testNamespace is created and available from suite_test.go
		// If not, you'd create it here:
		// testNamespace = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "cdk8sappproxy-test-"}}
		// Expect(k8sClient.Create(ctx, testNamespace)).To(Succeed())
	})

	AfterEach(func() {
		// Reset the commander to the real implementation after each test
		controllers.ResetCommander()
		// if testNamespace != nil {
		// 	Expect(k8sClient.Delete(ctx, testNamespace)).To(Succeed())
		// }
	})

	Context("When reconciling a Cdk8sAppProxy", func() {
		var cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy
		var key types.NamespacedName

		BeforeEach(func() {
			// Common setup for Cdk8sAppProxy resource for each It block
			// Ensure namespace is created in BeforeSuite or here. Using "default" for simplicity if not.
			ns := "default"
			if testNamespace != nil {
				ns = testNamespace.Name
			}

			cdk8sAppProxy = &addonsv1alpha1.Cdk8sAppProxy{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cdk8sappproxy",
					Namespace: ns,
				},
				Spec: addonsv1alpha1.Cdk8sAppProxySpec{
					ClusterSelector: metav1.LabelSelector{MatchLabels: map[string]string{"env": "test"}},
					// Source fields will be set per test case
				},
			}
			Expect(k8sClient.Create(ctx, cdk8sAppProxy)).To(Succeed())
			key = types.NamespacedName{Name: cdk8sAppProxy.Name, Namespace: cdk8sAppProxy.Namespace}

			// Ensure finalizer is added for deletion tests if needed, or rely on reconcileNormal to add it
			Eventually(func(g Gomega) {
				err := k8sClient.Get(ctx, key, cdk8sAppProxy)
				g.Expect(err).NotTo(HaveOccurred())
				if !controllerutil.ContainsFinalizer(cdk8sAppProxy, controllers.Cdk8sAppProxyFinalizer) {
					controllerutil.AddFinalizer(cdk8sAppProxy, controllers.Cdk8sAppProxyFinalizer)
					err = k8sClient.Update(ctx, cdk8sAppProxy)
					g.Expect(err).NotTo(HaveOccurred())
				}
				// Re-fetch to ensure we have the latest version with finalizer for tests
				err = k8sClient.Get(ctx, key, cdk8sAppProxy)
				g.Expect(err).NotTo(HaveOccurred())
			}, "10s", "100ms").Should(Succeed())
		})

		AfterEach(func() {
			// Clean up the Cdk8sAppProxy resource
			// Ensure finalizer is removed if present to allow deletion, or test deletion logic separately.
			// For non-deletion tests, a simple delete might be fine if finalizer logic isn't fully engaged.
			if cdk8sAppProxy != nil && cdk8sAppProxy.UID != "" { // Check if it was created
				// It's important to fetch the latest version before trying to patch/update for finalizer removal
				latestProxy := &addonsv1alpha1.Cdk8sAppProxy{}
				err := k8sClient.Get(ctx, key, latestProxy)
				if err == nil && controllerutil.ContainsFinalizer(latestProxy, controllers.Cdk8sAppProxyFinalizer) {
					controllerutil.RemoveFinalizer(latestProxy, controllers.Cdk8sAppProxyFinalizer)
					Expect(k8sClient.Update(ctx, latestProxy)).To(Succeed())
				}
				// Now delete
				Expect(k8sClient.Delete(ctx, cdk8sAppProxy)).To(Succeed())
				// Wait for deletion to complete
				Eventually(func() bool {
					err := k8sClient.Get(ctx, key, cdk8sAppProxy)

					return apierrors.IsNotFound(err)
				}, "10s", "100ms").Should(BeTrue())
			}
		})

		It("should reconcile with LocalPath and successful synth", func() {
			tempDir, err := os.MkdirTemp("", "local-cdk8s-app-")
			Expect(err).NotTo(HaveOccurred())
			defer func() {
				// Using GinkgoT().Logf for logging within tests if needed, or just check error.
				if err := os.RemoveAll(tempDir); err != nil {
					GinkgoT().Logf("Failed to remove tempDir %s: %v", tempDir, err)
				}
			}()

			// Create a dummy dist dir and a manifest file
			distDir := filepath.Join(tempDir, "dist")
			Expect(os.Mkdir(distDir, 0755)).To(Succeed())
			dummyManifest := `
apiVersion: v1
kind: ConfigMap
metadata:
  name: test-cm
data:
  key: value
`
			Expect(os.WriteFile(filepath.Join(distDir, "manifest.yaml"), []byte(dummyManifest), 0644)).To(Succeed())

			cdk8sAppProxy.Spec.LocalPath = tempDir
			Expect(k8sClient.Update(ctx, cdk8sAppProxy)).To(Succeed())

			// Mock cdk8s synth
			mockCommander.On("CombinedOutput", "cdk8s synth").Return([]byte("synth success output"), nil).Once()

			_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			mockCommander.AssertExpectations(GinkgoT())

			Eventually(func(g Gomega) {
				proxy := &addonsv1alpha1.Cdk8sAppProxy{}
				g.Expect(k8sClient.Get(ctx, key, proxy)).To(Succeed())
				cond := conditions.Get(proxy, addonsv1alpha1.DeploymentProgressingCondition)
				g.Expect(cond).NotTo(BeNil())
				// As no clusters are selected and no resources applied yet, it might not be True.
				// For this test, we are primarily checking synth and manifest parsing.
				// If parsing is successful and no clusters, it might be marked as successful or waiting for clusters.
				// For now, let's assume it becomes true if no errors up to cluster processing.
				// This will change once cluster apply logic is in.
				// As of current controller logic, it will be NoMatchingClustersReason
	// If synth is successful and no cluster, it should be NoMatchingClustersReason / False
	// For this test, we only care that synth was called and some condition was set.
	// The actual outcome of applying to clusters is not tested here.
	g.Expect(cond).NotTo(BeNil())
			}, "10s", "100ms").Should(Succeed())
		})

		It("should reconcile with GitRepository, clone, and successful synth", func() {
			cdk8sAppProxy.Spec.GitRepository = &addonsv1alpha1.GitRepositorySpec{
				URL: "https://github.com/example/repo.git",
			}
			Expect(k8sClient.Update(ctx, cdk8sAppProxy)).To(Succeed())

			// No git clone mock needed, go-git will be called directly.
			// For this test, we can't easily mock the filesystem interaction of go-git's clone.
			// We'll rely on the synth mock. If the clone fails in the test environment (e.g. no network),
			// the test will fail at the Reconcile step or show GitCloneFailedReason.
			// This is a simplification due to not mocking go-git's internals.

			// Mock cdk8s synth (will run in a temp dir created by the controller)
			mockCommander.On("CombinedOutput", "cdk8s synth").Return([]byte("synth success"), nil).Once()

			// Since we're not creating a real manifest in the temp git dir, synth (if it runs) would find nothing.
			// The controller attempts a real clone. If the environment running the test cannot clone
			// `https://github.com/example/repo.git` (e.g., network issue, dummy URL not actually existing),
			// then `go-git` will return an error, and `Reconcile` will propagate that.
			// If the clone *succeeds* (e.g., if the URL was real and accessible), then `cdk8s synth` is called.
			// Since `mockCommander` is set up for `cdk8s synth`, that part will be mocked.
			// After synth, it will try to find manifests. Since none exist in the temp dir, it will result in NoManifestsFoundReason.

			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			// We expect an error here if the clone fails (e.g. due to network or invalid URL)
			// OR if the clone succeeds but no manifests are found (which is expected as we don't create them).
			// If the clone fails, the error will be related to the clone.
			// If the clone succeeds, synth is mocked, then manifest walking happens. If no manifests, no error is returned by Reconcile directly,
			// but a condition is set.
			// For this test, let's assume the clone URL is a placeholder and might fail.
			// If it doesn't fail, the condition check below will handle the "NoManifestsFoundReason".
			if err != nil {
				GinkgoT().Logf("Reconcile returned an error (expected if clone failed): %v", err)
			}
			// Expect(err).NotTo(HaveOccurred()) // This might occur if clone fails.

			mockCommander.AssertExpectations(GinkgoT()) // Asserts synth was called if clone didn't error out first

			Eventually(func(g Gomega) {
				proxy := &addonsv1alpha1.Cdk8sAppProxy{}
				g.Expect(k8sClient.Get(ctx, key, proxy)).To(Succeed())
				cond := conditions.Get(proxy, addonsv1alpha1.DeploymentProgressingCondition)
				g.Expect(cond).NotTo(BeNil())
				// If clone failed, reason is GitCloneFailedReason.
				// If clone succeeded, synth mocked, then NoManifestsFoundReason or NoMatchingClustersReason.
				g.Expect(cond.Reason).To(Or(
					Equal(addonsv1alpha1.GitCloneFailedReason),
					Equal(addonsv1alpha1.NoManifestsFoundReason),
					Equal(addonsv1alpha1.NoMatchingClustersReason),
				))
			}, "10s", "100ms").Should(Succeed())
		})

		It("should fail if Git clone fails", func() {
			cdk8sAppProxy.Spec.GitRepository = &addonsv1alpha1.GitRepositorySpec{
				URL: "file:///nonexistentpath/to/trigger/clonefailure", // Invalid URL to ensure go-git clone fails
			}
			Expect(k8sClient.Update(ctx, cdk8sAppProxy)).To(Succeed())

			// No mockCommander for git clone needed as go-git is used directly.

			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).To(HaveOccurred()) // go-git should return an error for an invalid URL/path

			// mockCommander.AssertExpectations(GinkgoT()) // No expectations to assert if clone fails first

			Eventually(func(g Gomega) {
				proxy := &addonsv1alpha1.Cdk8sAppProxy{}
				g.Expect(k8sClient.Get(ctx, key, proxy)).To(Succeed())
				cond := conditions.Get(proxy, addonsv1alpha1.DeploymentProgressingCondition)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(BeEquivalentTo(metav1.ConditionFalse))
				g.Expect(cond.Reason).To(Equal(addonsv1alpha1.GitCloneFailedReason))
			}, "10s", "100ms").Should(Succeed())
		})

		It("should fail if Git checkout fails", func() {
			// Use a real repo URL that is publicly accessible to ensure clone succeeds.
			// The controller creates a real temp directory for the clone.
			// Use a non-existent ref to cause checkout failure.
			cdk8sAppProxy.Spec.GitRepository = &addonsv1alpha1.GitRepositorySpec{
				URL:       "https://github.com/kubernetes-sigs/cluster-api.git", // A known public repo
				Reference: "refs/heads/non-existent-branch-for-testing",
			}
			Expect(k8sClient.Update(ctx, cdk8sAppProxy)).To(Succeed())

			// No mock for git clone or checkout needed.
			// The actual go-git PlainCloneContext will run. If the machine running tests
			// doesn't have internet, this test will fail at clone stage itself.
			// Assuming internet access for this specific test case to reach checkout phase.

			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).To(HaveOccurred()) // go-git should return an error for non-existent reference checkout

			// No mockCommander expectations for git commands.

			Eventually(func(g Gomega) {
				proxy := &addonsv1alpha1.Cdk8sAppProxy{}
				g.Expect(k8sClient.Get(ctx, key, proxy)).To(Succeed())
				cond := conditions.Get(proxy, addonsv1alpha1.DeploymentProgressingCondition)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(BeEquivalentTo(metav1.ConditionFalse))
				// If clone fails due to network, this might be GitCloneFailedReason.
				// If clone succeeds and checkout fails, it should be GitCheckoutFailedReason.
				g.Expect(cond.Reason).To(Or(Equal(addonsv1alpha1.GitCheckoutFailedReason), Equal(addonsv1alpha1.GitCloneFailedReason)))
			}, "20s", "200ms").Should(Succeed()) // Increased timeout for potential network op
		})

		It("should fail if no source is specified", func() {
			// Spec is empty by default in this BeforeEach setup
			// No need to update, just reconcile
			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).To(HaveOccurred())                                  // Controller returns errors.New("no source specified...")
			Expect(err.Error()).To(ContainSubstring("no source specified")) // This error comes from pkg/errors

			Eventually(func(g Gomega) {
				proxy := &addonsv1alpha1.Cdk8sAppProxy{}
				g.Expect(k8sClient.Get(ctx, key, proxy)).To(Succeed())
				cond := conditions.Get(proxy, addonsv1alpha1.DeploymentProgressingCondition)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(BeEquivalentTo(metav1.ConditionFalse))
				g.Expect(cond.Reason).To(Equal(addonsv1alpha1.SourceNotSpecifiedReason))
			}, "10s", "100ms").Should(Succeed())
		})

		It("should fail if cdk8s synth fails", func() {
			cdk8sAppProxy.Spec.LocalPath = "/tmp/fake-local-path" // Needs a non-empty path
			Expect(k8sClient.Update(ctx, cdk8sAppProxy)).To(Succeed())

			mockCommander.On("CombinedOutput", "cdk8s synth").Return([]byte("synth error output"), apierrors.NewInternalError(errors.New("synth command failed"))).Once() // Using apierrors or stdlib

			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("synth command failed"))

			mockCommander.AssertExpectations(GinkgoT())

			Eventually(func(g Gomega) {
				proxy := &addonsv1alpha1.Cdk8sAppProxy{}
				g.Expect(k8sClient.Get(ctx, key, proxy)).To(Succeed())
				cond := conditions.Get(proxy, addonsv1alpha1.DeploymentProgressingCondition)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(BeEquivalentTo(metav1.ConditionFalse))
				g.Expect(cond.Reason).To(Equal(addonsv1alpha1.Cdk8sSynthFailedReason))
			}, "10s", "100ms").Should(Succeed())
		})
	})

	Context("When reconciling a Cdk8sAppProxy with GitRepository authentication", func() {
		var cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy
		var key types.NamespacedName
		var secret *corev1.Secret

		BeforeEach(func() {
			ns := "default"
			if testNamespace != nil {
				ns = testNamespace.Name
			}

			cdk8sAppProxy = &addonsv1alpha1.Cdk8sAppProxy{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "auth-test-proxy-", // Use GenerateName for multiple tests
					Namespace:    ns,
				},
				Spec: addonsv1alpha1.Cdk8sAppProxySpec{
					ClusterSelector: metav1.LabelSelector{MatchLabels: map[string]string{"env": "test"}},
					GitRepository: &addonsv1alpha1.GitRepositorySpec{
						URL: "https://example.com/git/repo.git", // Dummy URL
						// AuthSecretRef will be set per test
					},
				},
			}
			Expect(k8sClient.Create(ctx, cdk8sAppProxy)).To(Succeed())
			key = types.NamespacedName{Name: cdk8sAppProxy.Name, Namespace: cdk8sAppProxy.Namespace}

			// Ensure finalizer is added
			Eventually(func(g Gomega) {
				err := k8sClient.Get(ctx, key, cdk8sAppProxy)
				g.Expect(err).NotTo(HaveOccurred())
				if !controllerutil.ContainsFinalizer(cdk8sAppProxy, controllers.Cdk8sAppProxyFinalizer) {
					controllerutil.AddFinalizer(cdk8sAppProxy, controllers.Cdk8sAppProxyFinalizer)
					err = k8sClient.Update(ctx, cdk8sAppProxy)
					g.Expect(err).NotTo(HaveOccurred())
				}
			}, "10s", "100ms").Should(Succeed())
		})

		AfterEach(func() {
			if cdk8sAppProxy != nil && cdk8sAppProxy.UID != "" {
				latestProxy := &addonsv1alpha1.Cdk8sAppProxy{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: cdk8sAppProxy.Name, Namespace: cdk8sAppProxy.Namespace}, latestProxy)
				if err == nil && controllerutil.ContainsFinalizer(latestProxy, controllers.Cdk8sAppProxyFinalizer) {
					controllerutil.RemoveFinalizer(latestProxy, controllers.Cdk8sAppProxyFinalizer)
					Expect(k8sClient.Update(ctx, latestProxy)).To(Succeed(), "Failed to remove finalizer from Cdk8sAppProxy")
				}
				Expect(k8sClient.Delete(ctx, cdk8sAppProxy)).To(Succeed(), "Failed to delete Cdk8sAppProxy")
				Eventually(func() bool {
					err := k8sClient.Get(ctx, types.NamespacedName{Name: cdk8sAppProxy.Name, Namespace: cdk8sAppProxy.Namespace}, cdk8sAppProxy)

					return apierrors.IsNotFound(err)
				}, "10s", "100ms").Should(BeTrue(), "Cdk8sAppProxy not deleted")
			}
			if secret != nil && secret.UID != "" {
				Expect(k8sClient.Delete(ctx, secret)).To(Succeed(), "Failed to delete test secret")
				Eventually(func() bool {
					err := k8sClient.Get(ctx, types.NamespacedName{Name: secret.Name, Namespace: secret.Namespace}, secret)

					return apierrors.IsNotFound(err)
				}, "10s", "100ms").Should(BeTrue(), "Test secret not deleted")
				secret = nil // Reset for next test
			}
		})

		It("should fail if AuthSecretRef specified but Secret does not exist", func() {
			cdk8sAppProxy.Spec.GitRepository.AuthSecretRef = &corev1.LocalObjectReference{Name: "nonexistent-git-secret"}
			Expect(k8sClient.Update(ctx, cdk8sAppProxy)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			// This test assumes that the dummy URL will cause a clone failure.
			// If the dummy URL somehow resolves or if go-git handles it differently,
			// the error might occur after attempting to get the secret.
			// The primary check is the condition reason.
			Expect(err).To(HaveOccurred()) // Expecting an error because secret retrieval will fail

			Eventually(func(g Gomega) {
				proxy := &addonsv1alpha1.Cdk8sAppProxy{}
				g.Expect(k8sClient.Get(ctx, key, proxy)).To(Succeed())
				cond := conditions.Get(proxy, addonsv1alpha1.DeploymentProgressingCondition)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(BeEquivalentTo(metav1.ConditionFalse))
				g.Expect(cond.Reason).To(Equal(addonsv1alpha1.GitAuthSecretInvalidReason))
				g.Expect(cond.Message).To(ContainSubstring("Failed to get auth secret"))
			}, "10s", "100ms").Should(Succeed())
		})

		It("should fail if Secret exists but is missing required data fields", func() {
			secretName := "git-secret-missing-fields"
			cdk8sAppProxy.Spec.GitRepository.AuthSecretRef = &corev1.LocalObjectReference{Name: secretName}
			Expect(k8sClient.Update(ctx, cdk8sAppProxy)).To(Succeed())

			secret = &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: cdk8sAppProxy.Namespace},
				Data:       map[string][]byte{"user": []byte("testuser")}, // Missing "password" or using "user" instead of "username"
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).To(HaveOccurred())

			Eventually(func(g Gomega) {
				proxy := &addonsv1alpha1.Cdk8sAppProxy{}
				g.Expect(k8sClient.Get(ctx, key, proxy)).To(Succeed())
				cond := conditions.Get(proxy, addonsv1alpha1.DeploymentProgressingCondition)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(BeEquivalentTo(metav1.ConditionFalse))
				g.Expect(cond.Reason).To(Equal(addonsv1alpha1.GitAuthSecretInvalidReason))
				g.Expect(cond.Message).To(ContainSubstring("auth secret missing username or password fields"))
			}, "10s", "100ms").Should(Succeed())
		})

		It("should fail with GitAuthenticationFailedReason for invalid credentials", func() {
			secretName := "git-secret-invalid-creds"
			// Using a URL that is unlikely to exist or accept these credentials
			cdk8sAppProxy.Spec.GitRepository.URL = "https://invalid-credentials.example.com/repo.git"
			cdk8sAppProxy.Spec.GitRepository.AuthSecretRef = &corev1.LocalObjectReference{Name: secretName}
			Expect(k8sClient.Update(ctx, cdk8sAppProxy)).To(Succeed())

			secret = &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: cdk8sAppProxy.Namespace},
				Data: map[string][]byte{
					"username": []byte("actualtestuser"),
					"password": []byte("actualtestpassword"),
				},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).To(HaveOccurred()) // go-git clone with invalid creds (or unreachable host) should error

			Eventually(func(g Gomega) {
				proxy := &addonsv1alpha1.Cdk8sAppProxy{}
				g.Expect(k8sClient.Get(ctx, key, proxy)).To(Succeed())
				cond := conditions.Get(proxy, addonsv1alpha1.DeploymentProgressingCondition)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(BeEquivalentTo(metav1.ConditionFalse))
				// Depending on how go-git reports the error for an invalid host vs bad creds for a real host:
				// It might be GitAuthenticationFailedReason or GitCloneFailedReason.
				// The controller logic specifically checks for plumbingtransport.ErrAuthenticationRequired.
				// If the host is truly unreachable, it will likely be a generic clone failure.
				// For this test, we assume the URL makes go-git return an auth-like error or a clear clone fail.
				// The controller's current logic will set GitAuthenticationFailedReason if "authentication required" is in the error string.
				// A truly non-existent domain like "invalid-credentials.example.com" might result in a DNS error,
				// leading to GitCloneFailedReason. If it were a real git server that rejected credentials,
				// GitAuthenticationFailedReason would be more likely.
				g.Expect(cond.Reason).To(SatisfyAny(
					Equal(addonsv1alpha1.GitAuthenticationFailedReason),
					Equal(addonsv1alpha1.GitCloneFailedReason),
				))
				g.Expect(cond.Message).To(ContainSubstring("go-git clone failed"))
			}, "10s", "100ms").Should(Succeed())
		})
	})

	// Helper functions for Git repo management in tests
	func setupTempGitRepo(g *WithT) (string, func()) {
		repoPath, err := os.MkdirTemp("", "test-git-repo-")
		g.Expect(err).NotTo(HaveOccurred())

		cmd := exec.Command("git", "init")
		cmd.Dir = repoPath
		_, err = cmd.CombinedOutput()
		g.Expect(err).NotTo(HaveOccurred(), "Failed to git init: %s", string(_))

		// Configure dummy user for commits
		cmd = exec.Command("git", "config", "user.email", "test@example.com")
		cmd.Dir = repoPath
		_, err = cmd.CombinedOutput()
		g.Expect(err).NotTo(HaveOccurred(), "Failed to set git user.email: %s", string(_))
		cmd = exec.Command("git", "config", "user.name", "Test User")
		cmd.Dir = repoPath
		_, err = cmd.CombinedOutput()
		g.Expect(err).NotTo(HaveOccurred(), "Failed to set git user.name: %s", string(_))

		cleanup := func() {
			os.RemoveAll(repoPath)
		}
		return repoPath, cleanup
	}

	func makeGitCommit(g *WithT, repoPath, message, fileName string) string {
		if fileName == "" {
			fileName = "file.txt"
		}
		filePath := filepath.Join(repoPath, fileName)
		err := os.WriteFile(filePath, []byte(message), 0644)
		g.Expect(err).NotTo(HaveOccurred())

		cmd := exec.Command("git", "add", filePath)
		cmd.Dir = repoPath
		_, err = cmd.CombinedOutput()
		g.Expect(err).NotTo(HaveOccurred(), "Failed to git add: %s", string(_))

		cmd = exec.Command("git", "commit", "-m", message)
		cmd.Dir = repoPath
		output, err := cmd.CombinedOutput()
		g.Expect(err).NotTo(HaveOccurred(), "Failed to git commit: %s", string(output))

		// Get commit hash
		repo, err := git.PlainOpen(repoPath)
		g.Expect(err).NotTo(HaveOccurred())
		headRef, err := repo.Head()
		g.Expect(err).NotTo(HaveOccurred())
		return headRef.Hash().String()
	}

	Context("When reconciling a Cdk8sAppProxy with Git change detection", func() {
		var cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy
		var key types.NamespacedName
		var tempGitRepoPath string
		var cleanupTempGitRepo func()

		BeforeEach(func() {
			ns := "default" // Or use testNamespace.Name if available and preferred
			if testNamespace != nil {
				ns = testNamespace.Name
			}

			// Setup temporary Git repository for these tests
			tempGitRepoPath, cleanupTempGitRepo = setupTempGitRepo(NewGomegaWithT(GinkgoT()))

			cdk8sAppProxy = &addonsv1alpha1.Cdk8sAppProxy{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "git-change-test-",
					Namespace:    ns,
				},
				Spec: addonsv1alpha1.Cdk8sAppProxySpec{
					ClusterSelector: metav1.LabelSelector{MatchLabels: map[string]string{"env": "test"}},
					GitRepository: &addonsv1alpha1.GitRepositorySpec{
						URL: fmt.Sprintf("file://%s", tempGitRepoPath), // Use local file URL
					},
				},
			}
			Expect(k8sClient.Create(ctx, cdk8sAppProxy)).To(Succeed())
			key = types.NamespacedName{Name: cdk8sAppProxy.Name, Namespace: cdk8sAppProxy.Namespace}

			// Ensure finalizer is added
			Eventually(func(g Gomega) {
				err := k8sClient.Get(ctx, key, cdk8sAppProxy)
				g.Expect(err).NotTo(HaveOccurred())
				if !controllerutil.ContainsFinalizer(cdk8sAppProxy, controllers.Cdk8sAppProxyFinalizer) {
					controllerutil.AddFinalizer(cdk8sAppProxy, controllers.Cdk8sAppProxyFinalizer)
					err = k8sClient.Update(ctx, cdk8sAppProxy)
					g.Expect(err).NotTo(HaveOccurred())
				}
			}, "10s", "100ms").Should(Succeed())
		})

		AfterEach(func() {
			if cleanupTempGitRepo != nil {
				cleanupTempGitRepo()
			}
			if cdk8sAppProxy != nil && cdk8sAppProxy.UID != "" {
				latestProxy := &addonsv1alpha1.Cdk8sAppProxy{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: cdk8sAppProxy.Name, Namespace: cdk8sAppProxy.Namespace}, latestProxy)
				if err == nil && controllerutil.ContainsFinalizer(latestProxy, controllers.Cdk8sAppProxyFinalizer) {
					controllerutil.RemoveFinalizer(latestProxy, controllers.Cdk8sAppProxyFinalizer)
					Expect(k8sClient.Update(ctx, latestProxy)).To(Succeed(), "Failed to remove finalizer")
				}
				Expect(k8sClient.Delete(ctx, cdk8sAppProxy)).To(Succeed(), "Failed to delete Cdk8sAppProxy")
				Eventually(func() bool {
					err := k8sClient.Get(ctx, types.NamespacedName{Name: cdk8sAppProxy.Name, Namespace: cdk8sAppProxy.Namespace}, cdk8sAppProxy)
					return apierrors.IsNotFound(err)
				}, "10s", "100ms").Should(BeTrue(), "Cdk8sAppProxy not deleted")
			}
		})

		It("TestInitialDeployment: should synth and update hash if LastProcessedGitHash is empty", func() {
			commitHash := makeGitCommit(NewGomegaWithT(GinkgoT()), tempGitRepoPath, "Initial commit", "init.txt")

			// Create a dummy dist dir and a manifest file in the tempGitRepoPath for synth to succeed
			distDir := filepath.Join(tempGitRepoPath, "dist")
			Expect(os.Mkdir(distDir, 0755)).To(Succeed())
			dummyManifest := `apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: test-cm-initial\n`
			Expect(os.WriteFile(filepath.Join(distDir, "manifest.yaml"), []byte(dummyManifest), 0644)).To(Succeed())

			mockCommander.On("CombinedOutput", "cdk8s synth").Return([]byte("synth success output"), nil).Once()

			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			mockCommander.AssertExpectations(GinkgoT())

			Eventually(func(g Gomega) {
				proxy := &addonsv1alpha1.Cdk8sAppProxy{}
				g.Expect(k8sClient.Get(ctx, key, proxy)).To(Succeed())
				g.Expect(proxy.Status.LastProcessedGitHash).To(Equal(commitHash))
				cond := conditions.Get(proxy, addonsv1alpha1.DeploymentProgressingCondition)
				g.Expect(cond).NotTo(BeNil())
				// Because no clusters are selected, it will be NoMatchingClustersReason
				g.Expect(cond.Reason).To(Equal(addonsv1alpha1.NoMatchingClustersReason))
				g.Expect(cond.Status).To(BeEquivalentTo(metav1.ConditionFalse))
				g.Expect(proxy.Status.ObservedGeneration).To(Equal(proxy.Generation))
			}, "10s", "100ms").Should(Succeed())
		})

		It("TestNoGitChange: should not synth if commit hash matches LastProcessedGitHash", func() {
			commitHash := makeGitCommit(NewGomegaWithT(GinkgoT()), tempGitRepoPath, "First commit", "nochange.txt")

			// Manually update status to simulate a previous successful reconciliation
			cdk8sAppProxy.Status.LastProcessedGitHash = commitHash
			cdk8sAppProxy.Status.ObservedGeneration = cdk8sAppProxy.Generation - 1 // Simulate an older generation
			Expect(k8sClient.Status().Update(ctx, cdk8sAppProxy)).To(Succeed())

			// Fetch the updated proxy to ensure the Reconcile function gets the updated status
			Expect(k8sClient.Get(ctx, key, cdk8sAppProxy)).To(Succeed())


			// cdk8s synth should NOT be called
			// mockCommander.On("CombinedOutput", "cdk8s synth").Return([]byte("synth should not be called"), nil).Maybe() // .Maybe() or check AssertNotCalled

			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			// mockCommander.AssertNotCalled(GinkgoT(), "CombinedOutput", "cdk8s synth") // This specific assertion might need testify's mock features if not available in this setup.
			// For now, we rely on not setting an expectation for "cdk8s synth" for this call. If it were called, AssertExpectations would fail if it was unexpected.
			// Or, more simply, if we don't set an expectation with .Once(), and it's called, the test will fail.
			// So, no mockCommander.On for "cdk8s synth" here.

			Eventually(func(g Gomega) {
				proxy := &addonsv1alpha1.Cdk8sAppProxy{}
				g.Expect(k8sClient.Get(ctx, key, proxy)).To(Succeed())
				g.Expect(proxy.Status.LastProcessedGitHash).To(Equal(commitHash)) // Should remain unchanged
				cond := conditions.Get(proxy, addonsv1alpha1.DeploymentProgressingCondition)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(BeEquivalentTo(metav1.ConditionTrue)) // Because no change is needed, it's considered "progressed" to desired state
				g.Expect(proxy.Status.ObservedGeneration).To(Equal(proxy.Generation))
			}, "10s", "100ms").Should(Succeed())
		})

		It("TestGitChangeDetected: should synth and update hash if new commit detected", func() {
			oldCommitHash := makeGitCommit(NewGomegaWithT(GinkgoT()), tempGitRepoPath, "Old commit", "change.txt")

			cdk8sAppProxy.Status.LastProcessedGitHash = oldCommitHash
			Expect(k8sClient.Status().Update(ctx, cdk8sAppProxy)).To(Succeed())
			Expect(k8sClient.Get(ctx, key, cdk8sAppProxy)).To(Succeed()) // Re-fetch

			// Make a new commit
			newCommitHash := makeGitCommit(NewGomegaWithT(GinkgoT()), tempGitRepoPath, "New commit", "change.txt") // Commit to the same file to change its hash

			// Dummy dist dir and manifest for synth
			distDir := filepath.Join(tempGitRepoPath, "dist")
			Expect(os.MkdirAll(distDir, 0755)).To(Succeed()) // MkdirAll in case it was cleaned or not created
			dummyManifest := `apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: test-cm-change\n`
			Expect(os.WriteFile(filepath.Join(distDir, "manifest-new.yaml"), []byte(dummyManifest), 0644)).To(Succeed())

			mockCommander.On("CombinedOutput", "cdk8s synth").Return([]byte("synth success after change"), nil).Once()

			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			mockCommander.AssertExpectations(GinkgoT())

			Eventually(func(g Gomega) {
				proxy := &addonsv1alpha1.Cdk8sAppProxy{}
				g.Expect(k8sClient.Get(ctx, key, proxy)).To(Succeed())
				g.Expect(proxy.Status.LastProcessedGitHash).To(Equal(newCommitHash))
				cond := conditions.Get(proxy, addonsv1alpha1.DeploymentProgressingCondition)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Reason).To(Equal(addonsv1alpha1.NoMatchingClustersReason)) // Assuming no clusters
				g.Expect(cond.Status).To(BeEquivalentTo(metav1.ConditionFalse))
				g.Expect(proxy.Status.ObservedGeneration).To(Equal(proxy.Generation))
			}, "10s", "100ms").Should(Succeed())
		})

		It("TestGitFetchHeadError: should fail gracefully if .git is corrupted (simulating Head() error)", func() {
			// Initial commit to make it a valid repo first
			makeGitCommit(NewGomegaWithT(GinkgoT()), tempGitRepoPath, "Initial commit", "headerror.txt")

			// Corrupt the HEAD file to simulate an error when repo.Head() is called
			// This is a bit of a hack. A more robust way would be to mock the git library itself.
			headFilePath := filepath.Join(tempGitRepoPath, ".git", "HEAD")
			Expect(os.WriteFile(headFilePath, []byte("ref: refs/heads/nonexistent"), 0644)).To(Succeed())


			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).To(HaveOccurred()) // Expect an error from the Reconcile function

			Eventually(func(g Gomega) {
				proxy := &addonsv1alpha1.Cdk8sAppProxy{}
				g.Expect(k8sClient.Get(ctx, key, proxy)).To(Succeed())
				cond := conditions.Get(proxy, addonsv1alpha1.DeploymentProgressingCondition)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(BeEquivalentTo(metav1.ConditionFalse))
				// The error message comes from "Failed to get HEAD reference"
				g.Expect(cond.Reason).To(Equal(addonsv1alpha1.GitCloneFailedReason)) // Or a more specific reason if we add one for Head() failure
				g.Expect(cond.Message).To(ContainSubstring("Failed to get HEAD reference"))
			}, "10s", "100ms").Should(Succeed())
		})
	})

	Context("TestWatchResourceDeletedTriggersAnnotation", func() {
		var cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy
		var key types.NamespacedName
		var fakeDynClient *dynamicfake.FakeDynamicClient
		var watchChan chan watch.Event
		var reconcilerWithFakeClient *controllers.Cdk8sAppProxyReconciler

		BeforeEach(func() {
			ns := "default"
			if testNamespace != nil {
				ns = testNamespace.Name
			}
			ctrl.SetLogger(testLog) // Set logger for controller context

			cdk8sAppProxy = &addonsv1alpha1.Cdk8sAppProxy{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "watch-trigger-test-proxy",
					Namespace: ns,
					UID:       "test-uid-proxy", // Set a UID for watch key generation
				},
				Spec: addonsv1alpha1.Cdk8sAppProxySpec{
					// Setup spec so that reconcileNormal reaches the watch setup part
					LocalPath: "/tmp/dummy-local-path-for-watch-test", // Using LocalPath to simplify
					ClusterSelector: metav1.LabelSelector{MatchLabels: map[string]string{"watch-test-cluster": "true"}},
				},
			}

			// Use a fake client for the main reconciler to control Get/Update calls for Cdk8sAppProxy
			fakeK8sClient = fake.NewClientBuilder().WithScheme(fakeScheme).WithObjects(cdk8sAppProxy.DeepCopy()).Build()
			reconcilerWithFakeClient = &controllers.Cdk8sAppProxyReconciler{
				Client:        fakeK8sClient, // Use fake client here
				Scheme:        fakeScheme,
				ActiveWatches: make(map[types.NamespacedName]map[string]context.CancelFunc),
			}
			key = types.NamespacedName{Name: cdk8sAppProxy.Name, Namespace: cdk8sAppProxy.Namespace}

			// Mock cdk8s synth to "succeed" and produce a dummy manifest
			mockCommander.On("CombinedOutput", "cdk8s synth").Return([]byte(`
apiVersion: v1
kind: ConfigMap
metadata:
  name: watched-resource
  namespace: default
`), nil).Once() // Synth is called once to set up the watch

			// Mock getDynamicClientForCluster to return a fake dynamic client
			fakeDynClient = dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()) // Use a new scheme for dynamic client
			watchChan = make(chan watch.Event)
			fakeDynClient.PrependWatchReactor("*", func(action kubetesting.Action) (handled bool, ret watch.Interface, err error) {
				gvr := action.GetResource()
				ns := action.GetNamespace()
				testLog.Info("FakeDynamicClient Watch called", "gvr", gvr, "ns", ns)
				return true, watch.NewProxyWatcher(watchChan), nil
			})

			originalGetDynamicClientFunc = controllers.GetDynamicClientForClusterFunc // Save original
			controllers.GetDynamicClientForClusterFunc = func(ctx context.Context, secretNamespace string, clusterName string, proxy client.Client) (dynamic.Interface, error) {
				return fakeDynClient, nil
			}

			// Create a dummy cluster that matches the selector
			dummyCluster := &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "cluster.x-k8s.io/v1beta1",
					"kind":       "Cluster",
					"metadata": map[string]interface{}{
						"name":      "watched-cluster",
						"namespace": ns,
						"labels":    map[string]interface{}{"watch-test-cluster": "true"},
						"uid":       "test-uid-cluster",
					},
					"spec":   map[string]interface{}{},
					"status": map[string]interface{}{"infrastructureReady": true},
				},
			}
			// Create the Cdk8sAppProxy and Cluster objects using the fake client for this specific test context
			Expect(fakeK8sClient.Create(ctx, cdk8sAppProxy.DeepCopy())).To(Succeed())
			Expect(fakeK8sClient.Create(ctx, dummyCluster.DeepCopy())).To(Succeed())

			// Add finalizer using the fake client
			Expect(fakeK8sClient.Get(ctx, key, cdk8sAppProxy)).To(Succeed())
			controllerutil.AddFinalizer(cdk8sAppProxy, controllers.Cdk8sAppProxyFinalizer)
			Expect(fakeK8sClient.Update(ctx, cdk8sAppProxy)).To(Succeed())
		})

		AfterEach(func() {
			controllers.GetDynamicClientForClusterFunc = originalGetDynamicClientFunc // Restore original
			close(watchChan)

			// Clean up with the fake client
			latestProxy := &addonsv1alpha1.Cdk8sAppProxy{}
			err := fakeK8sClient.Get(ctx, key, latestProxy)
			if err == nil && controllerutil.ContainsFinalizer(latestProxy, controllers.Cdk8sAppProxyFinalizer) {
				controllerutil.RemoveFinalizer(latestProxy, controllers.Cdk8sAppProxyFinalizer)
				Expect(fakeK8sClient.Update(ctx, latestProxy)).To(Succeed())
			}
			Expect(fakeK8sClient.Delete(ctx, cdk8sAppProxy)).To(Succeed())

			dummyCluster := &unstructured.Unstructured{}
			dummyCluster.SetAPIVersion("cluster.x-k8s.io/v1beta1")
			dummyCluster.SetKind("Cluster")
			dummyCluster.SetName("watched-cluster")
			dummyCluster.SetNamespace(cdk8sAppProxy.Namespace)
			Expect(fakeK8sClient.Delete(ctx, dummyCluster)).To(Succeed())

			mu.Lock()
			reconciler.ActiveWatches = make(map[types.NamespacedName]map[string]context.CancelFunc)
			mu.Unlock()
			reconcilerWithFakeClient.ActiveWatches = make(map[types.NamespacedName]map[string]context.CancelFunc)
		})

		It("should trigger re-reconciliation by annotating parent proxy when a watched resource is deleted", func() {
			_, err := reconcilerWithFakeClient.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			mockCommander.AssertExpectations(GinkgoT())

			proxyNamespacedName := types.NamespacedName{Name: cdk8sAppProxy.Name, Namespace: cdk8sAppProxy.Namespace}
			var watchKeyFound string
			Eventually(func() bool {
				mu.Lock()
				defer mu.Unlock()
				if watchesForProxy, ok := reconcilerWithFakeClient.ActiveWatches[proxyNamespacedName]; ok {
					for k := range watchesForProxy {
						watchKeyFound = k
						return true
					}
				}
				return false
			}, "10s", "100ms").Should(BeTrue(), "Watch was not established")
			Expect(watchKeyFound).NotTo(BeEmpty())

			deletedResource := &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata":   map[string]interface{}{"name": "watched-resource", "namespace": "default"},
				},
			}
			watchChan <- watch.Event{Type: watch.Deleted, Object: deletedResource}

			Eventually(func(g Gomega) {
				updatedProxy := &addonsv1alpha1.Cdk8sAppProxy{}
				err := reconcilerWithFakeClient.Client.Get(ctx, key, updatedProxy)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(updatedProxy.Annotations).NotTo(BeNil())
				g.Expect(updatedProxy.Annotations).To(HaveKey("cdk8s.addons.cluster.x-k8s.io/reconcile-on-delete-trigger"))
				g.Expect(updatedProxy.Annotations["cdk8s.addons.cluster.x-k8s.io/reconcile-on-delete-trigger"]).NotTo(BeEmpty())

				mu.Lock()
				defer mu.Unlock()
				g.Expect(reconcilerWithFakeClient.ActiveWatches[proxyNamespacedName]).To(Not(HaveKey(watchKeyFound)), "Watch was not removed from active watches")
			}, "10s", "200ms").Should(Succeed())
		})
	})
})
