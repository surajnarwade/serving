/*
Copyright 2018 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package resources

import (
	"fmt"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/knative/pkg/kmeta"
	"github.com/knative/serving/pkg/activator"
	"github.com/knative/serving/pkg/apis/networking/v1alpha1"
	"github.com/knative/serving/pkg/apis/serving"
	servingv1alpha1 "github.com/knative/serving/pkg/apis/serving/v1alpha1"
	"github.com/knative/serving/pkg/reconciler"
	revisionresources "github.com/knative/serving/pkg/reconciler/v1alpha1/revision/resources"
	"github.com/knative/serving/pkg/reconciler/v1alpha1/route/config"
	"github.com/knative/serving/pkg/reconciler/v1alpha1/route/resources/names"
	"github.com/knative/serving/pkg/reconciler/v1alpha1/route/traffic"
	"github.com/knative/serving/pkg/system"
)

func isClusterLocal(r *servingv1alpha1.Route) bool {
	return strings.HasSuffix(r.Status.Domain, config.ClusterLocalDomain)
}

// MakeClusterIngress creates ClusterIngress to set up routing rules. Such ClusterIngress specifies
// which Hosts that it applies to, as well as the routing rules.
func MakeClusterIngress(r *servingv1alpha1.Route, tc *traffic.TrafficConfig) *v1alpha1.ClusterIngress {
	ci := &v1alpha1.ClusterIngress{
		ObjectMeta: metav1.ObjectMeta{
			// As ClusterIngress resource is cluster-scoped,
			// here we use GenerateName to avoid conflict.
			GenerateName: names.ClusterIngressPrefix(r),
			Labels: map[string]string{
				serving.RouteLabelKey:          r.Name,
				serving.RouteNamespaceLabelKey: r.Namespace,
			},
			OwnerReferences: []metav1.OwnerReference{*kmeta.NewControllerRef(r)},
			Annotations:     r.ObjectMeta.Annotations,
		},
		Spec: makeClusterIngressSpec(r, tc.Targets),
	}
	return ci
}

func makeClusterIngressSpec(r *servingv1alpha1.Route, targets map[string][]traffic.RevisionTarget) v1alpha1.IngressSpec {
	// Domain should have been specified in route status
	// before calling this func.
	domain := r.Status.Domain
	names := []string{}
	for name := range targets {
		names = append(names, name)
	}
	// Sort the names to give things a deterministic ordering.
	sort.Strings(names)
	// The routes are matching rule based on domain name to traffic split targets.
	rules := []v1alpha1.ClusterIngressRule{}
	for _, name := range names {
		rules = append(rules, *makeClusterIngressRule(getRouteDomains(name, r, domain), r.Namespace, targets[name]))
	}
	spec := v1alpha1.IngressSpec{
		Rules:      rules,
		Visibility: v1alpha1.IngressVisibilityExternalIP,
	}
	if isClusterLocal(r) {
		spec.Visibility = v1alpha1.IngressVisibilityClusterLocal
	}
	return spec
}

func getRouteDomains(targetName string, r *servingv1alpha1.Route, domain string) []string {
	if targetName == "" {
		// Nameless traffic targets correspond to many domains: the
		// Route.Status.Domain, and also various names of the Route's
		// headless Service.
		domains := []string{domain,
			names.K8sServiceFullname(r),
			fmt.Sprintf("%s.%s.svc", r.Name, r.Namespace),
			fmt.Sprintf("%s.%s", r.Name, r.Namespace),
		}
		return dedup(domains)
	}

	return []string{fmt.Sprintf("%s.%s", targetName, domain)}
}

// groupTargets group given targets into active ones and inactive ones.
func groupTargets(targets []traffic.RevisionTarget) (active []traffic.RevisionTarget, inactive []traffic.RevisionTarget) {
	for _, t := range targets {
		if t.Active {
			active = append(active, t)
		} else {
			inactive = append(inactive, t)
		}
	}
	return active, inactive
}

func makeClusterIngressRule(domains []string, ns string, targets []traffic.RevisionTarget) *v1alpha1.ClusterIngressRule {
	active, inactive := groupTargets(targets)
	splits := []v1alpha1.ClusterIngressBackendSplit{}
	for _, t := range active {
		if t.Percent == 0 {
			// Don't include 0% routes.
			continue
		}
		splits = append(splits, v1alpha1.ClusterIngressBackendSplit{
			ClusterIngressBackend: v1alpha1.ClusterIngressBackend{
				ServiceNamespace: ns,
				ServiceName:      reconciler.GetServingK8SServiceNameForObj(t.TrafficTarget.RevisionName),
				ServicePort:      intstr.FromInt(int(revisionresources.ServicePort)),
			},
			Percent: t.Percent,
		})
	}
	path := v1alpha1.HTTPClusterIngressPath{
		Splits: splits,
		// TODO(lichuqiang): #2201, plumbing to config timeout and retries.

	}
	path.SetDefaults()
	return &v1alpha1.ClusterIngressRule{
		Hosts: domains,
		HTTP: &v1alpha1.HTTPClusterIngressRuleValue{
			Paths: []v1alpha1.HTTPClusterIngressPath{
				*addInactive(&path, ns, inactive),
			},
		},
	}
}

// addInactive constructs Splits for the inactive targets, and add into given IngressPath.
func addInactive(r *v1alpha1.HTTPClusterIngressPath, ns string, inactive []traffic.RevisionTarget) *v1alpha1.HTTPClusterIngressPath {
	totalInactivePercent := 0
	maxInactiveTarget := traffic.RevisionTarget{}
	for _, t := range inactive {
		totalInactivePercent += t.Percent
		if t.Percent >= maxInactiveTarget.Percent {
			maxInactiveTarget = t
		}
	}
	if totalInactivePercent == 0 {
		// There is actually no inactive Revisions.
		return r
	}
	r.Splits = append(r.Splits, v1alpha1.ClusterIngressBackendSplit{
		ClusterIngressBackend: v1alpha1.ClusterIngressBackend{
			ServiceNamespace: system.Namespace,
			ServiceName:      activator.K8sServiceName,
			ServicePort:      intstr.FromInt(int(revisionresources.ServicePort)),
		},
		Percent: totalInactivePercent,
	})
	r.AppendHeaders = map[string]string{
		activator.RevisionHeaderName:      maxInactiveTarget.RevisionName,
		activator.RevisionHeaderNamespace: ns,
	}
	return r
}

func dedup(strs []string) []string {
	existed := make(map[string]struct{})
	unique := []string{}
	for _, s := range strs {
		if _, ok := existed[s]; !ok {
			existed[s] = struct{}{}
			unique = append(unique, s)
		}
	}
	return unique
}
