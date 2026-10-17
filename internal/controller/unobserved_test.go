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
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

// Each way a role query gives no authoritative role carries why, so that a
// member that turns unknown can be told apart: no answer, a refused token,
// an Instance Manager that reported no role, an answer that contradicts
// itself. The reason never carries the token (#438).
func TestProbeRole_SaysWhyThereIsNoRole(t *testing.T) {
	probe := func(rt http.RoundTripper) RoleObservation {
		p := &HTTPRoleProber{Client: &http.Client{Transport: rt}, Tokens: staticToken(refusedTok),
			Now: func() time.Time { return testNow }}
		return p.ProbeRole(context.Background(), c0, "ns")
	}
	cases := []struct {
		name string
		rt   http.RoundTripper
		want string
	}{
		{"no answer", unreachable{}, "connection refused"},
		{"a refused token", answerStatus(http.StatusUnauthorized), "status 401"},
		{"a server error", answerStatus(http.StatusInternalServerError), "status 500"},
		{"no role reported", answerWith{instancemanager.HAStatus{Role: instancemanager.RoleUnknown,
			Reason: "heartbeat status failed: context deadline exceeded"}},
			"heartbeat status failed: context deadline exceeded"},
		{"another member answers", answerWith{instancemanager.HAStatus{Current: c1,
			Role: instancemanager.RoleSlave, Nodes: []instancemanager.NodeState{node(c1, "slave")}}}, c1},
		{"a master whose server is not active", answerWith{haStatus(instancemanager.RoleMaster, false,
			node(c0, "master"))}, "registered_and_active"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := probe(c.rt)
			if !strings.Contains(o.Reason, c.want) {
				t.Errorf("reason = %q, want it to name %q", o.Reason, c.want)
			}
			if strings.Contains(o.Reason, refusedTok) {
				t.Errorf("reason %q carries the token", o.Reason)
			}
			if o.RequestID == "" {
				t.Errorf("the observation of a query that was sent has no request ID")
			}
		})
	}
	if o := probe(answerWith{haStatus(instancemanager.RoleSlave, false, node(c0, "slave"))}); o.Reason != "" {
		t.Errorf("an authoritative answer has the reason %q", o.Reason)
	}
}

// obsProber answers each member with the observation the test set for it.
type obsProber struct{ obs map[string]RoleObservation }

func (p *obsProber) ProbeRole(_ context.Context, podName, _ string) RoleObservation {
	return p.obs[podName]
}

// A member whose role was known and no longer is gets one log line that says
// why its observation does not count; a member that stays unknown, or keeps
// its role, gets none (#438).
func TestReconcileHAStatus_LogsWhyAMemberIsNoLongerObserved(t *testing.T) {
	cluster := haCluster("why")
	m0, m1, m2 := "why-0", "why-1", "why-2"
	prober := &obsProber{obs: map[string]RoleObservation{m0: slave(), m1: slave(), m2: master()}}
	r := &CubridClusterReconciler{Clock: func() time.Time { return testNow }, Prober: prober}
	var lines []string
	ctx := logf.IntoContext(context.Background(), funcr.New(func(_, line string) {
		if strings.Contains(line, `"event"="member_role_unobserved"`) {
			lines = append(lines, line)
		}
	}, funcr.Options{}))

	r.reconcileHAStatus(ctx, cluster, 3)
	if len(lines) != 0 {
		t.Fatalf("members whose roles are known were logged: %q", lines)
	}
	why := "the Instance Manager reported no role: heartbeat status failed: signal: killed"
	prober.obs[m0] = RoleObservation{Reachable: true, Role: databasev1alpha1.RoleUnknown, ObservedAt: testNow, Reason: why}
	prober.obs[m1] = RoleObservation{Reachable: true, Role: databasev1alpha1.RoleSlave, ObservedAt: testNow.Add(-time.Minute)}
	r.reconcileHAStatus(ctx, cluster, 3)
	if len(lines) != 2 {
		t.Fatalf("lines = %q, want one for %s and one for %s", lines, m0, m1)
	}
	for member, want := range map[string]string{m0: why, m1: "older than"} {
		found := false
		for _, line := range lines {
			if strings.Contains(line, `"member"="`+member+`"`) {
				found = true
				if !strings.Contains(line, want) || !strings.Contains(line, `"oldRole"="slave"`) {
					t.Errorf("line for %s = %s, want the reason %q and the old role", member, line, want)
				}
			}
		}
		if !found {
			t.Errorf("no line for %s: %q", member, lines)
		}
	}
	r.reconcileHAStatus(ctx, cluster, 3)
	if len(lines) != 2 {
		t.Errorf("a member that stays unknown was logged again: %q", lines[2:])
	}
}
