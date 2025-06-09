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
	"sync"

	addonsv1alpha1 "github.com/PatrickLaabs/cluster-api-addon-provider-cdk8s/api/v1alpha1"
	"github.com/PatrickLaabs/cluster-api-addon-provider-cdk8s/controllers"
	"github.com/go-git/go-git/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/mock"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubetesting "k8s.io/client-go/testing"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// MockCommander is a mock for the command execution part.
type MockCommander struct {
	mock.Mock
}

func (m *MockCommander) CombinedOutput(command string, args ...string) ([]byte, error) {
	var MOCK_COMMAND_TYPE string
	if command == "git" && len(args) > 0 && args[0] == "clone" {
		MOCK_COMMAND_TYPE = "git clone"
	} else if command == "git" && len(args) > 0 && args[0] == "checkout" {
		MOCK_COMMAND_TYPE = "git checkout"
	} else if command == "cdk8s" && len(args) > 0 && args[0] == "synth" {
		MOCK_COMMAND_TYPE = "cdk8s synth"
	} else {
		MOCK_COMMAND_TYPE = command
	}

	calledArgs := m.Called(MOCK_COMMAND_TYPE)
	output := calledArgs.Get(0)
	err := calledArgs.Error(1)

	if output == nil {
		return nil, err
	}
	byteOutput, ok := output.([]byte)
	if !ok && output != nil {
		return nil, errors.New("mock commander output was not of type []byte")
	}

	return byteOutput, err
}

var _ = Describe("Cdk8sAppProxy controller", func() {
	var (
		reconciler                   *controllers.Cdk8sAppProxyReconciler
		mockCommander                *MockCommander
		testNamespace                *corev1.Namespace
		ctx                          context.Context
		fakeK8sClient                client.Client
		fakeScheme                   *runtime.Scheme
		testLog                      = zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true))
		mu                           sync.Mutex
		originalGetDynamicClientFunc func(ctx context.Context, secretNamespace string, clusterName string, proxy client.Client) (dynamic.Interface, error)
	)

	BeforeEach(func() {
		ctx = context.Background()
		Expect(k8sClient).NotTo(BeNil(), "k8sClient should be initialized in BeforeSuite")

		mockCommander = new(MockCommander)
		controllers.SetCommander(func(command string, args ...string) controllers.CommandExecutor {
			return &controllers.RealCmdRunner{Name: command, Args: args, CommanderFunc: mockCommander.CombinedOutput}
		})

		reconciler = &controllers.Cdk8sAppProxyReconciler{
			Client:        k8sClient,
			Scheme:        k8sClient.Scheme(),
			ActiveWatches: make(map[types.NamespacedName]map[string]context.CancelFunc),
		}

		fakeScheme = runtime.NewScheme()
		Expect(addonsv1alpha1.AddToScheme(fakeScheme)).To(Succeed())
		Expect(corev1.AddToScheme(fakeScheme)).To(Succeed())
	})

	AfterEach(func() {
		controllers.ResetCommander()
	})

	Context("When reconciling a Cdk8sAppProxy", func() {
		var cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy
		var key types.NamespacedName

		BeforeEach(func() {
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
				},
			}
			Expect(k8sClient.Create(ctx, cdk8sAppProxy)).To(Succeed())
			key = types.NamespacedName{Name: cdk8sAppProxy.Name, Namespace: cdk8sAppProxy.Namespace}

			Eventually(func(g Gomega) {
				err := k8sClient.Get(ctx, key, cdk8sAppProxy)
				g.Expect(err).NotTo(HaveOccurred())
				if !controllerutil.ContainsFinalizer(cdk8sAppProxy, controllers.Cdk8sAppProxyFinalizer) {
					controllerutil.AddFinalizer(cdk8sAppProxy, controllers.Cdk8sAppProxyFinalizer)
					err = k8sClient.Update(ctx, cdk8sAppProxy)
					g.Expect(err).NotTo(HaveOccurred())
				}
				err = k8sClient.Get(ctx, key, cdk8sAppProxy)
				g.Expect(err).NotTo(HaveOccurred())
			}, "10s", "100ms").Should(Succeed())
		})

		AfterEach(func() {
			if cdk8sAppProxy != nil && cdk8sAppProxy.UID != "" {
				latestProxy := &addonsv1alpha1.Cdk8sAppProxy{}
				err := k8sClient.Get(ctx, key, latestProxy)
				if err == nil && controllerutil.ContainsFinalizer(latestProxy, controllers.Cdk8sAppProxyFinalizer) {
					controllerutil.RemoveFinalizer(latestProxy, controllers.Cdk8sAppProxyFinalizer)
					Expect(k8sClient.Update(ctx, latestProxy)).To(Succeed())
				}
				Expect(k8sClient.Delete(ctx, cdk8sAppProxy)).To(Succeed())
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
				if err := os.RemoveAll(tempDir); err != nil {
					GinkgoT().Logf("Failed to remove tempDir %s: %v", tempDir, err)
				}
			}()

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

			mockCommander.On("CombinedOutput", "cdk8s synth").Return([]byte("synth success output"), nil).Once()

			_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			mockCommander.AssertExpectations(GinkgoT())

			Eventually(func(g Gomega) {
				proxy := &addonsv1alpha1.Cdk8sAppProxy{}
				g.Expect(k8sClient.Get(ctx, key, proxy)).To(Succeed())
				cond := conditions.Get(proxy, addonsv1alpha1.DeploymentProgressingCondition)
				g.Expect(cond).NotTo(BeNil())
			}, "10s", "100ms").Should(Succeed())
		})

		It("should reconcile with GitRepository, clone, and successful synth", func() {
			cdk8sAppProxy.Spec.GitRepository = &addonsv1alpha1.GitRepositorySpec{
				URL: "https://github.com/example/repo.git",
			}
			Expect(k8sClient.Update(ctx, cdk8sAppProxy)).To(Succeed())

			mockCommander.On("CombinedOutput", "cdk8s synth").Return([]byte("synth success"), nil).Once()

			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			if err != nil {
				GinkgoT().Logf("Reconcile returned an error (expected if clone failed): %v", err)
			}

			mockCommander.AssertExpectations(GinkgoT())

			Eventually(func(g Gomega) {
				proxy := &addonsv1alpha1.Cdk8sAppProxy{}
				g.Expect(k8sClient.Get(ctx, key, proxy)).To(Succeed())
				cond := conditions.Get(proxy, addonsv1alpha1.DeploymentProgressingCondition)
				g.Expect(cond).NotTo(BeNil())
			}, "10s", "100ms").Should(Succeed())
		})

		It("should fail if Git clone fails", func() {
			cdk8sAppProxy.Spec.GitRepository = &addonsv1alpha1.GitRepositorySpec{
				URL: "file:///nonexistentpath/to/trigger/clonefailure",
			}
			Expect(k8sClient.Update(ctx, cdk8sAppProxy)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).To(HaveOccurred())

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
			cdk8sAppProxy.Spec.GitRepository = &addonsv1alpha1.GitRepositorySpec{
				URL:       "https://github.com/kubernetes-sigs/cluster-api.git",
				Reference: "refs/heads/non-existent-branch-for-testing",
			}
			Expect(k8sClient.Update(ctx, cdk8sAppProxy)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).To(HaveOccurred())

			Eventually(func(g Gomega) {
				proxy := &addonsv1alpha1.Cdk8sAppProxy{}
				g.Expect(k8sClient.Get(ctx, key, proxy)).To(Succeed())
				cond := conditions.Get(proxy, addonsv1alpha1.DeploymentProgressingCondition)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(BeEquivalentTo(metav1.ConditionFalse))
				g.Expect(cond.Reason).To(Or(Equal(addonsv1alpha1.GitCheckoutFailedReason), Equal(addonsv1alpha1.GitCloneFailedReason)))
			}, "20s", "200ms").Should(Succeed())
		})

		It("should fail if no source is specified", func() {
			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("no source specified"))

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
			cdk8sAppProxy.Spec.LocalPath = "/tmp/fake-local-path"
			Expect(k8sClient.Update(ctx, cdk8sAppProxy)).To(Succeed())

			mockCommander.On("CombinedOutput", "cdk8s synth").Return([]byte("synth error output"), apierrors.NewInternalError(errors.New("synth command failed"))).Once()

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
					GenerateName: "auth-test-proxy-",
					Namespace:    ns,
				},
				Spec: addonsv1alpha1.Cdk8sAppProxySpec{
					ClusterSelector: metav1.LabelSelector{MatchLabels: map[string]string{"env": "test"}},
					GitRepository: &addonsv1alpha1.GitRepositorySpec{
						URL: "https://example.com/git/repo.git",
					},
				},
			}
			Expect(k8sClient.Create(ctx, cdk8sAppProxy)).To(Succeed())
			key = types.NamespacedName{Name: cdk8sAppProxy.Name, Namespace: cdk8sAppProxy.Namespace}

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
				secret = nil
			}
		})

		It("should fail if AuthSecretRef specified but Secret does not exist", func() {
			cdk8sAppProxy.Spec.GitRepository.AuthSecretRef = &corev1.LocalObjectReference{Name: "nonexistent-git-secret"}
			Expect(k8sClient.Update(ctx, cdk8sAppProxy)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).To(HaveOccurred())

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
				Data:       map[string][]byte{"user": []byte("testuser")},
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
			Expect(err).To(HaveOccurred())

			Eventually(func(g Gomega) {
				proxy := &addonsv1alpha1.Cdk8sAppProxy{}
				g.Expect(k8sClient.Get(ctx, key, proxy)).To(Succeed())
				cond := conditions.Get(proxy, addonsv1alpha1.DeploymentProgressingCondition)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(BeEquivalentTo(metav1.ConditionFalse))
				g.Expect(cond.Reason).To(SatisfyAny(
					Equal(addonsv1alpha1.GitAuthenticationFailedReason),
					Equal(addonsv1alpha1.GitCloneFailedReason),
				))
				g.Expect(cond.Message).To(ContainSubstring("go-git clone failed"))
			}, "10s", "100ms").Should(Succeed())
		})
	})

	Context("When reconciling a Cdk8sAppProxy with Git change detection", func() {
		var cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy
		var key types.NamespacedName
		var tempGitRepoPath string
		var cleanupTempGitRepo func()

		BeforeEach(func() {
			ns := "default"

			tempGitRepoPath, cleanupTempGitRepo = setupTempGitRepo(NewGomegaWithT(GinkgoT()))

			cdk8sAppProxy = &addonsv1alpha1.Cdk8sAppProxy{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "git-change-test-",
					Namespace:    ns,
				},
				Spec: addonsv1alpha1.Cdk8sAppProxySpec{
					ClusterSelector: metav1.LabelSelector{MatchLabels: map[string]string{"env": "test"}},
					GitRepository: &addonsv1alpha1.GitRepositorySpec{
						URL: fmt.Sprintf("file://%s", tempGitRepoPath),
					},
				},
			}
			Expect(k8sClient.Create(ctx, cdk8sAppProxy)).To(Succeed())
			key = types.NamespacedName{Name: cdk8sAppProxy.Name, Namespace: cdk8sAppProxy.Namespace}

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
				g.Expect(cond.Reason).To(Equal(addonsv1alpha1.NoMatchingClustersReason))
				g.Expect(cond.Status).To(BeEquivalentTo(metav1.ConditionFalse))
				g.Expect(proxy.Status.ObservedGeneration).To(Equal(proxy.Generation))
			}, "10s", "100ms").Should(Succeed())
		})

		It("TestNoGitChange: should not synth if commit hash matches LastProcessedGitHash", func() {
			commitHash := makeGitCommit(NewGomegaWithT(GinkgoT()), tempGitRepoPath, "First commit", "nochange.txt")

			cdk8sAppProxy.Status.LastProcessedGitHash = commitHash
			cdk8sAppProxy.Status.ObservedGeneration = cdk8sAppProxy.Generation - 1
			Expect(k8sClient.Status().Update(ctx, cdk8sAppProxy)).To(Succeed())

			Expect(k8sClient.Get(ctx, key, cdk8sAppProxy)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				proxy := &addonsv1alpha1.Cdk8sAppProxy{}
				g.Expect(k8sClient.Get(ctx, key, proxy)).To(Succeed())
				g.Expect(proxy.Status.LastProcessedGitHash).To(Equal(commitHash))
				cond := conditions.Get(proxy, addonsv1alpha1.DeploymentProgressingCondition)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(BeEquivalentTo(metav1.ConditionTrue))
				g.Expect(proxy.Status.ObservedGeneration).To(Equal(proxy.Generation))
			}, "10s", "100ms").Should(Succeed())
		})

		It("TestGitChangeDetected: should synth and update hash if new commit detected", func() {
			oldCommitHash := makeGitCommit(NewGomegaWithT(GinkgoT()), tempGitRepoPath, "Old commit", "change.txt")

			cdk8sAppProxy.Status.LastProcessedGitHash = oldCommitHash
			Expect(k8sClient.Status().Update(ctx, cdk8sAppProxy)).To(Succeed())
			Expect(k8sClient.Get(ctx, key, cdk8sAppProxy)).To(Succeed())

			newCommitHash := makeGitCommit(NewGomegaWithT(GinkgoT()), tempGitRepoPath, "New commit", "change.txt")

			distDir := filepath.Join(tempGitRepoPath, "dist")
			Expect(os.MkdirAll(distDir, 0755)).To(Succeed())
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
				g.Expect(cond.Reason).To(Equal(addonsv1alpha1.NoMatchingClustersReason))
				g.Expect(cond.Status).To(BeEquivalentTo(metav1.ConditionFalse))
				g.Expect(proxy.Status.ObservedGeneration).To(Equal(proxy.Generation))
			}, "10s", "100ms").Should(Succeed())
		})

		It("TestGitFetchHeadError: should fail gracefully if .git is corrupted (simulating Head() error)", func() {
			makeGitCommit(NewGomegaWithT(GinkgoT()), tempGitRepoPath, "Initial commit", "headerror.txt")

			headFilePath := filepath.Join(tempGitRepoPath, ".git", "HEAD")
			Expect(os.WriteFile(headFilePath, []byte("ref: refs/heads/nonexistent"), 0644)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).To(HaveOccurred())

			Eventually(func(g Gomega) {
				proxy := &addonsv1alpha1.Cdk8sAppProxy{}
				g.Expect(k8sClient.Get(ctx, key, proxy)).To(Succeed())
				cond := conditions.Get(proxy, addonsv1alpha1.DeploymentProgressingCondition)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(BeEquivalentTo(metav1.ConditionFalse))
				g.Expect(cond.Reason).To(Equal(addonsv1alpha1.GitCloneFailedReason))
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
			ctrl.SetLogger(testLog)

			cdk8sAppProxy = &addonsv1alpha1.Cdk8sAppProxy{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "watch-trigger-test-proxy",
					Namespace: ns,
					UID:       "test-uid-proxy",
				},
				Spec: addonsv1alpha1.Cdk8sAppProxySpec{
					LocalPath:       "/tmp/dummy-local-path-for-watch-test",
					ClusterSelector: metav1.LabelSelector{MatchLabels: map[string]string{"watch-test-cluster": "true"}},
				},
			}

			fakeK8sClient = fake.NewClientBuilder().WithScheme(fakeScheme).WithObjects(cdk8sAppProxy.DeepCopy()).Build()
			reconcilerWithFakeClient = &controllers.Cdk8sAppProxyReconciler{
				Client:        fakeK8sClient,
				Scheme:        fakeScheme,
				ActiveWatches: make(map[types.NamespacedName]map[string]context.CancelFunc),
			}
			key = types.NamespacedName{Name: cdk8sAppProxy.Name, Namespace: cdk8sAppProxy.Namespace}

			mockCommander.On("CombinedOutput", "cdk8s synth").Return([]byte(`
apiVersion: v1
kind: ConfigMap
metadata:
  name: watched-resource
  namespace: default
`), nil).Once()

			fakeDynClient = dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
			watchChan = make(chan watch.Event)
			fakeDynClient.PrependWatchReactor("*", func(action kubetesting.Action) (handled bool, ret watch.Interface, err error) {
				gvr := action.GetResource()
				ns := action.GetNamespace()
				testLog.Info("FakeDynamicClient Watch called", "gvr", gvr, "ns", ns)
				return true, watch.NewProxyWatcher(watchChan), nil
			})

			originalGetDynamicClientFunc = controllers.GetDynamicClientForClusterFunc
			controllers.GetDynamicClientForClusterFunc = func(ctx context.Context, secretNamespace string, clusterName string, proxy client.Client) (dynamic.Interface, error) {
				return fakeDynClient, nil
			}

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

			Expect(fakeK8sClient.Create(ctx, cdk8sAppProxy.DeepCopy())).To(Succeed())
			Expect(fakeK8sClient.Create(ctx, dummyCluster.DeepCopy())).To(Succeed())

			Expect(fakeK8sClient.Get(ctx, key, cdk8sAppProxy)).To(Succeed())
			controllerutil.AddFinalizer(cdk8sAppProxy, controllers.Cdk8sAppProxyFinalizer)
			Expect(fakeK8sClient.Update(ctx, cdk8sAppProxy)).To(Succeed())
		})

		AfterEach(func() {
			controllers.GetDynamicClientForClusterFunc = originalGetDynamicClientFunc
			close(watchChan)

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

	Context("When handling finalizers", func() {
		var cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy
		var key types.NamespacedName

		BeforeEach(func() {
			ns := "default"
			if testNamespace != nil {
				ns = testNamespace.Name
			}

			cdk8sAppProxy = &addonsv1alpha1.Cdk8sAppProxy{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "finalizer-test-proxy",
					Namespace: ns,
				},
				Spec: addonsv1alpha1.Cdk8sAppProxySpec{
					LocalPath:       "/tmp/test-path",
					ClusterSelector: metav1.LabelSelector{MatchLabels: map[string]string{"env": "test"}},
				},
			}
			Expect(k8sClient.Create(ctx, cdk8sAppProxy)).To(Succeed())
			key = types.NamespacedName{Name: cdk8sAppProxy.Name, Namespace: cdk8sAppProxy.Namespace}
		})

		AfterEach(func() {
			if cdk8sAppProxy != nil && cdk8sAppProxy.UID != "" {
				latestProxy := &addonsv1alpha1.Cdk8sAppProxy{}
				err := k8sClient.Get(ctx, key, latestProxy)
				if err == nil && controllerutil.ContainsFinalizer(latestProxy, controllers.Cdk8sAppProxyFinalizer) {
					controllerutil.RemoveFinalizer(latestProxy, controllers.Cdk8sAppProxyFinalizer)
					Expect(k8sClient.Update(ctx, latestProxy)).To(Succeed())
				}
				Expect(k8sClient.Delete(ctx, cdk8sAppProxy)).To(Succeed())
			}
		})

		It("should add finalizer on first reconcile", func() {
			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				proxy := &addonsv1alpha1.Cdk8sAppProxy{}
				g.Expect(k8sClient.Get(ctx, key, proxy)).To(Succeed())
				g.Expect(controllerutil.ContainsFinalizer(proxy, controllers.Cdk8sAppProxyFinalizer)).To(BeTrue())
			}, "10s", "100ms").Should(Succeed())
		})

		It("should handle deletion and cleanup watches", func() {
			mockCommander.On("CombinedOutput", "cdk8s synth").Return([]byte("synth success"), nil).Once()
			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, key, cdk8sAppProxy)).To(Succeed())
			Expect(k8sClient.Delete(ctx, cdk8sAppProxy)).To(Succeed())

			_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() bool {
				err := k8sClient.Get(ctx, key, cdk8sAppProxy)
				return apierrors.IsNotFound(err)
			}, "10s", "100ms").Should(BeTrue())

			Expect(reconciler.ActiveWatches).NotTo(HaveKey(key))
		})
	})

	Context("When testing cluster selection", func() {
		var cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy
		var cluster1, cluster2, cluster3 *unstructured.Unstructured
		var key types.NamespacedName

		BeforeEach(func() {
			ns := "default"
			if testNamespace != nil {
				ns = testNamespace.Name
			}

			cluster1 = &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "cluster.x-k8s.io/v1beta1",
					"kind":       "Cluster",
					"metadata": map[string]interface{}{
						"name":      "cluster-env-prod",
						"namespace": ns,
						"labels":    map[string]interface{}{"env": "prod", "tier": "frontend"},
					},
					"status": map[string]interface{}{"infrastructureReady": true},
				},
			}
			cluster2 = &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "cluster.x-k8s.io/v1beta1",
					"kind":       "Cluster",
					"metadata": map[string]interface{}{
						"name":      "cluster-env-staging",
						"namespace": ns,
						"labels":    map[string]interface{}{"env": "staging", "tier": "backend"},
					},
					"status": map[string]interface{}{"infrastructureReady": true},
				},
			}
			cluster3 = &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "cluster.x-k8s.io/v1beta1",
					"kind":       "Cluster",
					"metadata": map[string]interface{}{
						"name":      "cluster-no-env",
						"namespace": ns,
						"labels":    map[string]interface{}{"tier": "database"},
					},
					"status": map[string]interface{}{"infrastructureReady": false},
				},
			}

			Expect(k8sClient.Create(ctx, cluster1)).To(Succeed())
			Expect(k8sClient.Create(ctx, cluster2)).To(Succeed())
			Expect(k8sClient.Create(ctx, cluster3)).To(Succeed())

			tempDir, err := os.MkdirTemp("", "cluster-selector-test-")
			Expect(err).NotTo(HaveOccurred())
			distDir := filepath.Join(tempDir, "dist")
			Expect(os.Mkdir(distDir, 0755)).To(Succeed())
			dummyManifest := `apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: test-cm\n`
			Expect(os.WriteFile(filepath.Join(distDir, "manifest.yaml"), []byte(dummyManifest), 0644)).To(Succeed())

			cdk8sAppProxy = &addonsv1alpha1.Cdk8sAppProxy{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cluster-selector-test",
					Namespace: ns,
				},
				Spec: addonsv1alpha1.Cdk8sAppProxySpec{
					LocalPath: tempDir,
				},
			}
			Expect(k8sClient.Create(ctx, cdk8sAppProxy)).To(Succeed())
			key = types.NamespacedName{Name: cdk8sAppProxy.Name, Namespace: cdk8sAppProxy.Namespace}

			DeferCleanup(func() {
				os.RemoveAll(tempDir)
			})
		})

		AfterEach(func() {
			resources := []*unstructured.Unstructured{cluster1, cluster2, cluster3}
			for _, resource := range resources {
				if resource != nil {
					Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
				}
			}

			if cdk8sAppProxy != nil {
				latestProxy := &addonsv1alpha1.Cdk8sAppProxy{}
				err := k8sClient.Get(ctx, key, latestProxy)
				if err == nil && controllerutil.ContainsFinalizer(latestProxy, controllers.Cdk8sAppProxyFinalizer) {
					controllerutil.RemoveFinalizer(latestProxy, controllers.Cdk8sAppProxyFinalizer)
					Expect(k8sClient.Update(ctx, latestProxy)).To(Succeed())
				}
				Expect(k8sClient.Delete(ctx, cdk8sAppProxy)).To(Succeed())
			}
		})

		It("should select clusters matching single label", func() {
			cdk8sAppProxy.Spec.ClusterSelector = metav1.LabelSelector{
				MatchLabels: map[string]string{"env": "prod"},
			}
			Expect(k8sClient.Update(ctx, cdk8sAppProxy)).To(Succeed())

			mockCommander.On("CombinedOutput", "cdk8s synth").Return([]byte("synth success"), nil).Once()

			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				proxy := &addonsv1alpha1.Cdk8sAppProxy{}
				g.Expect(k8sClient.Get(ctx, key, proxy)).To(Succeed())
				g.Expect(proxy.Status.SelectedClusters).To(HaveLen(1))
				g.Expect(proxy.Status.SelectedClusters[0]).To(Equal("cluster-env-prod"))
			}, "10s", "100ms").Should(Succeed())
		})

		It("should select clusters matching multiple labels", func() {
			cdk8sAppProxy.Spec.ClusterSelector = metav1.LabelSelector{
				MatchLabels: map[string]string{"tier": "backend"},
			}
			Expect(k8sClient.Update(ctx, cdk8sAppProxy)).To(Succeed())

			mockCommander.On("CombinedOutput", "cdk8s synth").Return([]byte("synth success"), nil).Once()

			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				proxy := &addonsv1alpha1.Cdk8sAppProxy{}
				g.Expect(k8sClient.Get(ctx, key, proxy)).To(Succeed())
				g.Expect(proxy.Status.SelectedClusters).To(HaveLen(1))
				g.Expect(proxy.Status.SelectedClusters[0]).To(Equal("cluster-env-staging"))
			}, "10s", "100ms").Should(Succeed())
		})

		It("should handle empty selector (select all ready clusters)", func() {
			cdk8sAppProxy.Spec.ClusterSelector = metav1.LabelSelector{}
			Expect(k8sClient.Update(ctx, cdk8sAppProxy)).To(Succeed())

			mockCommander.On("CombinedOutput", "cdk8s synth").Return([]byte("synth success"), nil).Once()

			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				proxy := &addonsv1alpha1.Cdk8sAppProxy{}
				g.Expect(k8sClient.Get(ctx, key, proxy)).To(Succeed())
				g.Expect(proxy.Status.SelectedClusters).To(HaveLen(2))
				g.Expect(proxy.Status.SelectedClusters).To(ContainElements("cluster-env-prod", "cluster-env-staging"))
			}, "10s", "100ms").Should(Succeed())
		})
	})
})

// Helper functions for Git repo management in tests
func setupTempGitRepo(g *WithT) (string, func()) {
	repoPath, err := os.MkdirTemp("", "test-git-repo-")
	g.Expect(err).NotTo(HaveOccurred())
	cmd := exec.Command("git", "init")
	cmd.Dir = repoPath
	output, err := cmd.CombinedOutput()
	g.Expect(err).NotTo(HaveOccurred(), "Failed to git init: %s", string(output))

	cmd = exec.Command("git", "config", "user.email", "test@example.com")
	cmd.Dir = repoPath
	output, err = cmd.CombinedOutput()
	g.Expect(err).NotTo(HaveOccurred(), "Failed to set git user.email: %s", string(output))

	cmd = exec.Command("git", "config", "user.name", "Test User")
	cmd.Dir = repoPath
	output, err = cmd.CombinedOutput()
	g.Expect(err).NotTo(HaveOccurred(), "Failed to set git user.name: %s", string(output))

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
	output, err := cmd.CombinedOutput()
	g.Expect(err).NotTo(HaveOccurred(), "Failed to git add: %s", string(output))

	cmd = exec.Command("git", "commit", "-m", message)
	cmd.Dir = repoPath
	output, err = cmd.CombinedOutput()
	g.Expect(err).NotTo(HaveOccurred(), "Failed to git commit: %s", string(output))

	repo, err := git.PlainOpen(repoPath)
	g.Expect(err).NotTo(HaveOccurred())
	headRef, err := repo.Head()
	g.Expect(err).NotTo(HaveOccurred())
	return headRef.Hash().String()
}
