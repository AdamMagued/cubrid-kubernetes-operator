/*
Copyright 2026.

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

package controller

import (
	"context"
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// A CubridCluster created before the name rule existed keeps its immutable
// name. Installing the CRD with the rule must not block updates to it, or
// neither users nor the operator could maintain it, while a new object with
// such a name is still refused (#459).
var _ = Describe("Cluster name validation across a CRD upgrade (#459)", func() {
	ctx := context.Background()

	const crdName = "cubridclusters.database.cubrid.io"
	crdGVK := schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}
	versionsPath := []string{"spec", "versions"}
	rulesPath := []string{"schema", "openAPIV3Schema", "x-kubernetes-validations"}

	getCRD := func() *unstructured.Unstructured {
		crd := &unstructured.Unstructured{}
		crd.SetGroupVersionKind(crdGVK)
		ExpectWithOffset(2, k8sClient.Get(ctx, client.ObjectKey{Name: crdName}, crd)).To(Succeed())
		return crd
	}
	// getRootRules and setRootRules read and replace the root
	// x-kubernetes-validations of the installed CRD's only version.
	getRootRules := func() []any {
		versions, _, err := unstructured.NestedSlice(getCRD().Object, versionsPath...)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		ExpectWithOffset(1, versions).To(HaveLen(1))
		rules, _, err := unstructured.NestedSlice(versions[0].(map[string]any), rulesPath...)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		return rules
	}
	setRootRules := func(rules []any) {
		crd := getCRD()
		versions, _, err := unstructured.NestedSlice(crd.Object, versionsPath...)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		version := versions[0].(map[string]any)
		if len(rules) == 0 {
			unstructured.RemoveNestedField(version, rulesPath...)
		} else {
			ExpectWithOffset(1, unstructured.SetNestedSlice(version, rules, rulesPath...)).To(Succeed())
		}
		ExpectWithOffset(1, unstructured.SetNestedSlice(crd.Object, versions, versionsPath...)).To(Succeed())
		ExpectWithOffset(1, k8sClient.Update(ctx, crd)).To(Succeed())
	}

	It("allows updates to an existing CubridCluster with a too-long name and refuses new ones", func() {
		installed := getRootRules()

		// The CRD before #459 differs from this one only by the root rule on
		// metadata.name.
		var legacy []any
		for _, r := range installed {
			if !strings.Contains(fmt.Sprint(r.(map[string]any)["rule"]), "metadata.name") {
				legacy = append(legacy, r)
			}
		}
		Expect(legacy).To(HaveLen(len(installed) - 1))

		probe := standaloneCluster(strings.Repeat("p", databasev1alpha1.MaxClusterNameLength+1))
		admitted := func() error { return k8sClient.Create(ctx, probe.DeepCopy(), client.DryRunAll) }

		setRootRules(legacy)
		DeferCleanup(func() { setRootRules(installed) })
		Eventually(admitted).Should(Succeed())

		existing := standaloneCluster(strings.Repeat("l", 60))
		Expect(k8sClient.Create(ctx, existing)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, existing))).To(Succeed()) })

		maintain := func(step string) {
			c := &databasev1alpha1.CubridCluster{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(existing), c)).To(Succeed())

			base := c.DeepCopy()
			c.Annotations = map[string]string{"example.com/maintained": step}
			Expect(k8sClient.Patch(ctx, c, client.MergeFrom(base))).To(Succeed(), "annotation patch "+step)

			base = c.DeepCopy()
			c.Spec.Resources.Requests = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}
			if step == "after" {
				c.Spec.Resources.Requests[corev1.ResourceCPU] = resource.MustParse("200m")
			}
			Expect(k8sClient.Patch(ctx, c, client.MergeFrom(base))).To(Succeed(), "spec patch "+step)

			base = c.DeepCopy()
			c.Status.ObservedGeneration = c.Generation
			Expect(k8sClient.Status().Patch(ctx, c, client.MergeFrom(base))).To(Succeed(), "status patch "+step)
		}
		maintain("before")

		setRootRules(installed)
		Eventually(admitted).Should(MatchError(ContainSubstring("at most 52 characters")))

		maintain("after")
		Expect(k8sClient.Create(ctx, standaloneCluster(strings.Repeat("n", 60)))).
			To(MatchError(ContainSubstring("at most 52 characters")))
	})
})
