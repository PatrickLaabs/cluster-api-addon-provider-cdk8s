/*
Copyright 2022 The Kubernetes Authors.

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

package internal

import (
	"bytes"
	"context"
	"text/template"

	"github.com/Masterminds/sprig/v3"
	addonsv1alpha1 "github.com/PatrickLaabs/cluster-api-addon-provider-cdk8s/api/v1alpha1"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/controllers/external"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlClient "sigs.k8s.io/controller-runtime/pkg/client"
)

// initializeBuiltins takes a map of keys to object references, attempts to get the referenced objects, and returns a map of keys to the actual objects.
// These objects are a map[string]interface{} so that they can be used as values in the template.
func initializeBuiltins(ctx context.Context, c ctrlClient.Client, referenceMap map[string]corev1.ObjectReference, cluster *clusterv1.Cluster) (map[string]interface{}, error) {
	log := ctrl.LoggerFrom(ctx)

	valueLookUp := make(map[string]interface{})

	for name, ref := range referenceMap {
		if ref.Namespace == "" {
			ref.Namespace = cluster.Namespace
		}
		log.V(2).Info("Getting object for reference", "ref", ref)
		obj, err := external.Get(ctx, c, &ref)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to get object %s", ref.Name)
		}
		valueLookUp[name] = obj.Object
	}

	return valueLookUp, nil
}

// ParseValues parses the values template and returns the expanded template. It attempts to populate a map of supported templating objects.
func ParseValues(ctx context.Context, c ctrlClient.Client, spec addonsv1alpha1.Cdk8sAppProxySpec, cluster *clusterv1.Cluster) (string, error) {
	log := ctrl.LoggerFrom(ctx)

	log.V(2).Info("Rendering templating in values:", "values", spec.Values) // Assuming Cdk8sAppProxySpec.Values holds the template string
	references := map[string]corev1.ObjectReference{
		"Cluster": {
			APIVersion: cluster.APIVersion,
			Kind:       cluster.Kind,
			Namespace:  cluster.Namespace,
			Name:       cluster.Name,
		},
	}

	if cluster.Spec.ControlPlaneRef != nil {
		references["ControlPlane"] = *cluster.Spec.ControlPlaneRef
	}
	if cluster.Spec.InfrastructureRef != nil {
		references["InfraCluster"] = *cluster.Spec.InfrastructureRef
	}
	// TODO: would we want to add ControlPlaneMachineTemplate?

	valueLookUp, err := initializeBuiltins(ctx, c, references, cluster)
	if err != nil {
		return "", err
	}

	tmpl, err := template.New(cluster.GetName()). // ChartName is not available in Cdk8sAppProxySpec, using cluster name for template name
							Funcs(sprig.TxtFuncMap()).
							Parse(spec.Values) // Assuming Cdk8sAppProxySpec.Values holds the template string
	if err != nil {
		return "", err
	}
	var buffer bytes.Buffer

	if err := tmpl.Execute(&buffer, valueLookUp); err != nil {
		return "", errors.Wrapf(err, "error executing template string '%s' on cluster '%s'", spec.Values, cluster.GetName())
	}
	expandedTemplate := buffer.String()
	log.V(2).Info("Expanded values to", "result", expandedTemplate)

	return expandedTemplate, nil
}
