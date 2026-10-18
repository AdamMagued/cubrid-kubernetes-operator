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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// A CUBRID cluster name must fit within the Kubernetes Service name limit
// (63 characters) when appended with the 10-character governing Service
// suffix "-instances". The API refuses cluster names longer than 53
// characters or names that violate RFC 1035 DNS label constraints (#459).
var _ = Describe("Cluster name validation (#459)", func() {
	ctx := context.Background()

	dryRun := func(c *databasev1alpha1.CubridCluster) error {
		return k8sClient.Create(ctx, c, client.DryRunAll)
	}

	It("accepts a 53-character name and refuses a 54-character name in a standalone CubridCluster", func() {
		longest := strings.Repeat("a", 53)
		tooLong := strings.Repeat("a", 54)

		Expect(dryRun(standaloneCluster(longest))).To(Succeed())
		Expect(dryRun(standaloneCluster(tooLong))).
			To(MatchError(ContainSubstring("53")))
	})

	It("accepts a 53-character name and refuses a 54-character or 61-character name in an HA CubridCluster", func() {
		longest := strings.Repeat("a", 53)
		tooLong54 := strings.Repeat("a", 54)
		tooLong61 := strings.Repeat("a", 61)

		Expect(dryRun(haCluster(longest))).To(Succeed())
		Expect(dryRun(haCluster(tooLong54))).
			To(MatchError(ContainSubstring("53")))
		Expect(dryRun(haCluster(tooLong61))).
			To(MatchError(ContainSubstring("53")))
	})

	It("refuses cluster names with invalid DNS-1035 syntax", func() {
		// Dots are valid in generic CR metadata.name (DNS subdomain) but invalid in Service names.
		Expect(dryRun(standaloneCluster("prod.example"))).
			To(MatchError(ContainSubstring("DNS-1035")))
		// Must start with an alphabetic character.
		Expect(dryRun(standaloneCluster("1cluster"))).
			To(MatchError(ContainSubstring("DNS-1035")))
		// Must not end with a hyphen.
		Expect(dryRun(standaloneCluster("cluster-"))).
			To(MatchError(ContainSubstring("DNS-1035")))
		// Must not contain uppercase characters.
		Expect(dryRun(standaloneCluster("Cluster"))).
			To(MatchError(ContainSubstring("DNS-1035")))
		// Must not contain underscores.
		Expect(dryRun(standaloneCluster("cluster_name"))).
			To(MatchError(ContainSubstring("DNS-1035")))
	})

	It("accepts valid short and standard kebab-case cluster names", func() {
		Expect(dryRun(standaloneCluster("c"))).To(Succeed())
		Expect(dryRun(standaloneCluster("my-cubrid-01"))).To(Succeed())
		Expect(dryRun(haCluster("my-cubrid-ha"))).To(Succeed())
	})

	It("verifies that all child resource names for the 53-character boundary conform to Kubernetes limits", func() {
		name := strings.Repeat("a", 53)
		ha := haCluster(name)

		// Governing headless Service name (<cluster>-instances).
		govSvc := instancesServiceName(name)
		Expect(len(govSvc)).To(Equal(63))
		Expect(validation.IsDNS1035Label(govSvc)).To(BeEmpty())

		// Per-member alias Service names (<cluster>-<ordinal>).
		members, err := haMemberNames(ha)
		Expect(err).NotTo(HaveOccurred())
		Expect(members).To(HaveLen(3))
		for _, m := range members {
			Expect(len(m)).To(Equal(55))
			Expect(validation.IsDNS1035Label(m)).To(BeEmpty())
			Expect(validation.IsDNS1123Label(m)).To(BeEmpty())
		}

		// IM token secret (<cluster>-im-token).
		imSecret := imTokenSecretName(name)
		Expect(len(imSecret)).To(Equal(62))
		Expect(validation.IsDNS1123Subdomain(imSecret)).To(BeEmpty())

		// HA ConfigMap (<cluster>-ha-config).
		haCM := haConfigMapName(name)
		Expect(len(haCM)).To(Equal(63))
		Expect(validation.IsDNS1123Subdomain(haCM)).To(BeEmpty())

		// NetworkPolicies.
		Expect(len(databaseNetworkPolicyName(name))).To(Equal(62))
		Expect(len(brokerNetworkPolicyName(name))).To(Equal(60))

		// Broker resources.
		Expect(len(brokerConfigMapName(name))).To(Equal(67))
		Expect(validation.IsDNS1123Subdomain(brokerConfigMapName(name))).To(BeEmpty())
		Expect(len(brokerDeploymentName(name, "rw"))).To(Equal(63))
		Expect(validation.IsDNS1123Subdomain(brokerDeploymentName(name, "rw"))).To(BeEmpty())
		Expect(len(rwServiceName(name))).To(Equal(56))
		Expect(validation.IsDNS1035Label(rwServiceName(name))).To(BeEmpty())
		Expect(len(roServiceName(name))).To(Equal(56))
		Expect(validation.IsDNS1035Label(roServiceName(name))).To(BeEmpty())

		// StatefulSet name and PVC volumeClaimTemplate naming: data-<cluster>-0.
		pvcName := "data-" + name + "-0"
		Expect(len(pvcName)).To(Equal(60))
		Expect(validation.IsDNS1123Subdomain(pvcName)).To(BeEmpty())
	})
})
