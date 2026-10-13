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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// clockProber reports one master and slaves at the test's clock; a member in
// unreachable gives no answer.
type clockProber struct {
	master      string
	clock       *time.Time
	unreachable map[string]bool
	appliers    map[string]ReplicationObservation
}

func (p *clockProber) ProbeRole(_ context.Context, podName, _ string) RoleObservation {
	if p.unreachable[podName] {
		return RoleObservation{Reachable: false, ObservedAt: *p.clock}
	}
	role := databasev1alpha1.RoleSlave
	if podName == p.master {
		role = databasev1alpha1.RoleMaster
	}
	o := RoleObservation{Reachable: true, Role: role, ObservedAt: *p.clock}
	if applier, ok := p.appliers[podName]; ok && role == databasev1alpha1.RoleSlave {
		o.Replication = &applier
	}
	return o
}

// The status says how old its HA observation is, and a time that only moves
// forward is not written on every pass (docs/observability.md, "Freshness").
var _ = Describe("Last observation time in the status (#370)", func() {
	ctx := context.Background()

	type fixture struct {
		key    types.NamespacedName
		r      *CubridClusterReconciler
		prober *clockProber
		clock  *time.Time
	}
	newFixture := func(name string) *fixture {
		Expect(k8sClient.Create(ctx, haCluster(name))).To(Succeed())
		key := types.NamespacedName{Name: name, Namespace: metav1.NamespaceDefault}
		DeferCleanup(func() {
			c := &databasev1alpha1.CubridCluster{}
			Expect(k8sClient.Get(ctx, key, c)).To(Succeed())
			Expect(k8sClient.Delete(ctx, c)).To(Succeed())
		})
		// Whole seconds: the API server stores a time to the second.
		clock := time.Now().Truncate(time.Second)
		prober := &clockProber{master: name + "-0", clock: &clock, unreachable: map[string]bool{},
			appliers: map[string]ReplicationObservation{
				name + "-1": {Source: name + "-0", AppliedChanges: 100},
				name + "-2": {Source: name + "-0", AppliedChanges: 100},
			}}
		r := &CubridClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(),
			Recorder: record.NewFakeRecorder(100), Prober: prober, Clock: func() time.Time { return clock }}
		return &fixture{key: key, r: r, prober: prober, clock: &clock}
	}
	// pass reconciles once and returns the stored cluster.
	pass := func(f *fixture) *databasev1alpha1.CubridCluster {
		GinkgoHelper()
		_, err := f.r.Reconcile(ctx, reconcile.Request{NamespacedName: f.key})
		Expect(err).NotTo(HaveOccurred())
		got := &databasev1alpha1.CubridCluster{}
		Expect(k8sClient.Get(ctx, f.key, got)).To(Succeed())
		return got
	}
	at := func(t *metav1.Time) time.Time {
		GinkgoHelper()
		Expect(t).NotTo(BeNil())
		return t.Time
	}
	// expectTimes asserts the observation times of the cluster, its members
	// and its slaves' appliers.
	expectTimes := func(c *databasev1alpha1.CubridCluster, want time.Time) {
		GinkgoHelper()
		Expect(at(c.Status.LastObservedTime)).To(BeTemporally("==", want), "status.lastObservedTime")
		for _, in := range c.Status.Instances {
			Expect(at(in.LastObservedTime)).To(BeTemporally("==", want), in.Name)
			if in.Replication != nil {
				Expect(at(in.Replication.ObservedAt)).To(BeTemporally("==", want), in.Name+" replication")
			}
		}
	}

	It("is set on the first observation and written again only after the write interval", func() {
		f := newFixture("obs-interval")
		start := *f.clock

		By("the first observation")
		c := pass(f)
		expectTimes(c, start)
		Expect(c.Status.Instances[1].Replication).NotTo(BeNil(), "the slave's applier reading")
		c = pass(f)
		version := c.ResourceVersion

		By("reconciles within the interval in which nothing else changes")
		for elapsed := haResyncInterval; elapsed < observationWriteInterval; elapsed += haResyncInterval {
			*f.clock = start.Add(elapsed)
			c = pass(f)
			Expect(c.ResourceVersion).To(Equal(version), "the status was written after %s", elapsed)
			expectTimes(c, start)
		}

		By("the first reconcile once the interval has passed")
		*f.clock = start.Add(observationWriteInterval)
		c = pass(f)
		Expect(c.ResourceVersion).NotTo(Equal(version), "the status was not written after the interval")
		expectTimes(c, *f.clock)
		version = c.ResourceVersion

		By("the next reconcile within the new interval")
		*f.clock = f.clock.Add(haResyncInterval)
		c = pass(f)
		Expect(c.ResourceVersion).To(Equal(version))
	})

	It("carries the current times when the status is written for another reason", func() {
		f := newFixture("obs-other-change")
		pass(f)

		By("a failover within the interval")
		*f.clock = f.clock.Add(haResyncInterval)
		f.prober.master = f.key.Name + "-1"
		c := pass(f)
		Expect(c.Status.CurrentPrimary).To(Equal(f.key.Name + "-1"))
		expectTimes(c, *f.clock)
	})

	It("keeps the time of a member that does not answer while the others advance", func() {
		f := newFixture("obs-unreachable")
		start := *f.clock
		pass(f)

		By("one slave stops answering")
		silent := f.key.Name + "-2"
		f.prober.unreachable[silent] = true
		*f.clock = start.Add(haResyncInterval)
		c := pass(f)
		Expect(at(c.Status.LastObservedTime)).To(BeTemporally("==", *f.clock))
		for _, in := range c.Status.Instances {
			want := *f.clock
			if in.Name == silent {
				want = start
			}
			Expect(at(in.LastObservedTime)).To(BeTemporally("==", want), in.Name)
		}

		By("it stays silent past the interval")
		*f.clock = start.Add(observationWriteInterval + haResyncInterval)
		c = pass(f)
		Expect(at(c.Status.LastObservedTime)).To(BeTemporally("==", *f.clock))
		Expect(at(c.Status.Instances[2].LastObservedTime)).To(BeTemporally("==", start))
		Expect(at(c.Status.Instances[0].LastObservedTime)).To(BeTemporally("==", *f.clock))
	})

	It("is neither advanced nor cleared by a reconcile that does not observe the roles", func() {
		f := newFixture("obs-not-observed")
		start := *f.clock
		pass(f)

		By("a member Service the operator does not own, so that the reconcile ends early")
		member := types.NamespacedName{Name: f.key.Name + "-1", Namespace: f.key.Namespace}
		owned := &corev1.Service{}
		Expect(k8sClient.Get(ctx, member, owned)).To(Succeed())
		Expect(k8sClient.Delete(ctx, owned)).To(Succeed())
		foreign := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: member.Name, Namespace: member.Namespace},
			Spec: corev1.ServiceSpec{
				Selector: map[string]string{"app.kubernetes.io/managed-by": "another-tool"}, Ports: []corev1.ServicePort{{Port: 80}},
			},
		}
		Expect(k8sClient.Create(ctx, foreign)).To(Succeed())
		*f.clock = start.Add(observationWriteInterval + haResyncInterval)
		_, err := f.r.Reconcile(ctx, reconcile.Request{NamespacedName: f.key})
		Expect(err).To(HaveOccurred())
		c := &databasev1alpha1.CubridCluster{}
		Expect(k8sClient.Get(ctx, f.key, c)).To(Succeed())
		expectRolesNotObserved(c.Status)
		expectTimes(c, start)

		By("the roles observed again")
		Expect(k8sClient.Delete(ctx, foreign)).To(Succeed())
		c = pass(f)
		expectTimes(c, *f.clock)
	})

	It("is not set for a cluster without a role prober", func() {
		f := newFixture("obs-no-prober")
		f.r.Prober = nil
		c := pass(f)
		Expect(c.Status.LastObservedTime).To(BeNil())
	})
})
