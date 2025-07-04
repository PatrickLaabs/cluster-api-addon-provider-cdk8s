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

	addonsv1alpha1 "github.com/PatrickLaabs/cluster-api-addon-provider-cdk8s/api/v1alpha1"
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
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

func (r *Reconciler) checkIfResourceExists(ctx context.Context, dynClient dynamic.Interface, gvr schema.GroupVersionResource, namespace string, name string) (bool, error) {
	resourceGetter := dynClient.Resource(gvr)
	if namespace != "" {
		_, err := resourceGetter.Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
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

func (r *Reconciler) synthesizeAndParseResources(appSourcePath string, logger logr.Logger) ([]*unstructured.Unstructured, error) {
	reason := addonsv1alpha1.Cdk8sSynthFailedReason
	// Synthesize cdk8s application
	if err := r.synthesizeCdk8sApp(appSourcePath, logger, reason); err != nil {
		return nil, err
	}

	// Find manifest files
	manifestFiles, err := r.findManifestFiles(appSourcePath, logger, reason)
	if err != nil {
		return nil, err
	}

	// Parse resources from manifest files using the consolidated function
	return r.parseManifestFiles(manifestFiles, logger, reason)
}

func (r *Reconciler) synthesizeCdk8sApp(appSourcePath string, logger logr.Logger, operation string) error {
	logger.Info("Synthesizing cdk8s application", "effectiveSourcePath", appSourcePath, "operation", operation)

	// npmInstall := cmdRunnerFactory("npm", "install")
	// npmInstall.SetDir(appSourcePath)
	// output, err := npmInstall.CombinedOutput()
	// if err != nil {
	//	logger.Error(err, "npm installation failed", "output", string(output), "operation:", OperationNpmInstall)
	// }

	synth := exec.Command("cdk8s", "synth")
	synth.Dir = appSourcePath
	if err := synth.Run(); err != nil {
		logger.Error(err, "Failed to synth cdk8s application", "effectiveSourcePath", appSourcePath)

		return err
	}

	logger.Info("Synthesized cdk8s application", "effectiveSourcePath", appSourcePath, "operation", operation)

	return nil
}

func (r *Reconciler) findManifestFiles(appSourcePath string, logger logr.Logger, operation string) ([]string, error) {
	distPath := filepath.Join(appSourcePath, "dist")

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
		logger.Error(walkErr, "Failed to walk dist directory", "operation", operation)

		return nil, walkErr
	}

	logger.Info("Found manifest files", "count", len(manifestFiles), "operation", operation)

	return manifestFiles, nil
}

func (r *Reconciler) parseManifestFiles(manifestFiles []string, logger logr.Logger, operation string) ([]*unstructured.Unstructured, error) {
	var parsedResources []*unstructured.Unstructured

	for _, manifestFile := range manifestFiles {
		logger.Info("Processing manifest file", "file", manifestFile, "operation", operation)

		fileContent, readErr := os.ReadFile(manifestFile)
		if readErr != nil {
			logger.Error(readErr, "Failed to read manifest file", "file", manifestFile, "operation", operation)

			return nil, readErr
		}

		yamlDecoder := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(fileContent), 100)

		for {
			var rawObj runtime.RawExtension
			if err := yamlDecoder.Decode(&rawObj); err != nil {
				if err.Error() == "EOF" {
					break
				}
				logger.Error(err, "Failed to decode YAML from manifest file", "file", manifestFile, "operation", operation)

				return nil, err
			}

			if rawObj.Raw == nil {
				continue
			}

			u := &unstructured.Unstructured{}
			if _, _, err := unstructured.UnstructuredJSONScheme.Decode(rawObj.Raw, nil, u); err != nil {
				logger.Error(err, "Failed to decode RawExtension to Unstructured", "file", manifestFile, "operation", operation)

				return nil, err
			}

			parsedResources = append(parsedResources, u)
			logger.Info("Parsed resource", "GVK", u.GroupVersionKind().String(), "Name", u.GetName(), "Namespace", u.GetNamespace(), "operation", operation)
		}
	}

	logger.Info("Total resources parsed", "count", len(parsedResources), "operation", operation)

	return parsedResources, nil
}

func (r *Reconciler) finalizeDeletion(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, proxyNamespacedName types.NamespacedName, logger logr.Logger) error {
	logger.Info("Starting finalization process")

	// Cancel any active watches for this Cdk8sAppProxy using the new WatchManager
	logger.Info("Cleaning up active watches for Cdk8sAppProxy before deletion")
	r.WatchManager.CleanupWatches(proxyNamespacedName)
	logger.Info("Completed cleanup of active watches")

	// Remove finalizer
	logger.Info("Finished deletion logic, removing finalizer")
	controllerutil.RemoveFinalizer(cdk8sAppProxy, Finalizer)
	if err := r.Update(ctx, cdk8sAppProxy); err != nil {
		logger.Error(err, "Failed to remove finalizer")

		return err
	}
	logger.Info("Finalizer removed successfully")

	return nil
}

func (r *Reconciler) getDynamicClientForCluster(ctx context.Context, secretNamespace, clusterName string) (dynamic.Interface, error) {
	logger := log.FromContext(ctx).WithValues("secretNamespace", secretNamespace, "clusterName", clusterName)
	kubeconfigSecretName := clusterName + "-kubeconfig"

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
	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		logger.Error(err, "Failed to create dynamic client")

		return nil, errors.Wrapf(err, "failed to create dynamic client for cluster %s", clusterName)
	}
	logger.Info("Successfully created dynamic client")

	return dynamicClient, nil
}

// Consolidated error handling.
func (r *Reconciler) updateStatusWithError(ctx context.Context, cdk8sAppProxy *addonsv1alpha1.Cdk8sAppProxy, reason, message string, err error, removeFinalizer bool) error {
	logger := log.FromContext(ctx).WithValues("cdk8sappproxy", types.NamespacedName{Name: cdk8sAppProxy.Name, Namespace: cdk8sAppProxy.Namespace})

	if err != nil {
		logger.Error(err, message, "reason", reason)
		conditions.MarkFalse(cdk8sAppProxy, addonsv1alpha1.DeploymentProgressingCondition, reason, clusterv1.ConditionSeverityError, "%s: %v", message, err)
	} else {
		logger.Info(message, "reason", reason)
	}

	if removeFinalizer {
		controllerutil.RemoveFinalizer(cdk8sAppProxy, Finalizer)
		if updateErr := r.Update(ctx, cdk8sAppProxy); updateErr != nil {
			logger.Error(updateErr, "Failed to remove finalizer after error")

			return updateErr
		}
		logger.Info("Removed finalizer after error/condition")
	} else {
		if statusUpdateErr := r.Status().Update(ctx, cdk8sAppProxy); statusUpdateErr != nil {
			logger.Error(statusUpdateErr, "Failed to update status after error")
		}
	}

	return err
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
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Initialize the new simplified watch system
	r.WatchManager = NewResourceWatchManager(mgr.GetClient())

	return ctrl.NewControllerManagedBy(mgr).
		For(&addonsv1alpha1.Cdk8sAppProxy{}).
		Watches(&clusterv1.Cluster{},
			handler.EnqueueRequestsFromMapFunc(r.ClusterToCdk8sAppProxyMapper)).
		Complete(r)
}

// ClusterToCdk8sAppProxyMapper is a handler.ToRequestsFunc to be used to enqeue requests for Cdk8sAppProxyReconciler.
// It maps CAPI Cluster events to Cdk8sAppProxy events.
func (r *Reconciler) ClusterToCdk8sAppProxyMapper(ctx context.Context, o client.Object) []ctrl.Request {
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
	// For this example, let's assume a cluster-wide list for Cdk8sAppProxy.
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
