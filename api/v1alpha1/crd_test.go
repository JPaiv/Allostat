package v1alpha1_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/JPaiv/Allostat/api/v1alpha1"
)

// These tests load the generated CRD files into a real API server and check
// that it enforces the rules. They need the test binaries: run `make test`.
func TestCRDs(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set; run `make test`")
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start the API server: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	meta := func(name string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, Namespace: "default"}
	}
	mustFail := func(t *testing.T, err error, want string) {
		t.Helper()
		if err == nil {
			t.Fatalf("want an error with %q, got none", want)
		}
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}

	contract := func(name string) *v1alpha1.Contract {
		return &v1alpha1.Contract{ObjectMeta: meta(name), Spec: v1alpha1.ContractSpec{
			Action:   "restart",
			Requires: []v1alpha1.Requirement{{Condition: "PeerCanServe", MaxAge: metav1.Duration{Duration: time.Minute}}},
			Budgets:  []v1alpha1.Budget{{Label: "shard", Max: 1}},
			GrantTTL: metav1.Duration{Duration: 10 * time.Minute},
		}}
	}
	observation := func(name string) *v1alpha1.Observation {
		return &v1alpha1.Observation{ObjectMeta: meta(name), Spec: v1alpha1.ObservationSpec{
			Condition: "PeerCanServe", Subject: "a", Status: metav1.ConditionTrue,
			Source: "replica-health", ObservedAt: metav1.NewMicroTime(time.Now()),
		}}
	}
	request := func(name string) *v1alpha1.ActionRequest {
		return &v1alpha1.ActionRequest{ObjectMeta: meta(name), Spec: v1alpha1.ActionRequestSpec{
			Action: "restart", Requester: "upgrade",
			Target: v1alpha1.Target{Name: "a", Labels: map[string]string{"shard": "1"}},
		}}
	}

	t.Run("valid objects are accepted and read back", func(t *testing.T) {
		in := contract("ok")
		if err := c.Create(ctx, in); err != nil {
			t.Fatal(err)
		}
		var out v1alpha1.Contract
		if err := c.Get(ctx, client.ObjectKeyFromObject(in), &out); err != nil {
			t.Fatal(err)
		}
		if out.Spec.GrantTTL.Duration != 10*time.Minute || out.Spec.Requires[0].MaxAge.Duration != time.Minute {
			t.Fatalf("durations changed: %+v", out.Spec)
		}
		if err := c.Create(ctx, observation("ok")); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("contract with a negative budget is rejected", func(t *testing.T) {
		bad := contract("negative")
		bad.Spec.Budgets[0].Max = -1
		mustFail(t, c.Create(ctx, bad), "spec.budgets[0].max")
	})

	t.Run("contract with the same condition twice is rejected", func(t *testing.T) {
		bad := contract("twice")
		bad.Spec.Requires = append(bad.Spec.Requires, bad.Spec.Requires[0])
		mustFail(t, c.Create(ctx, bad), "Duplicate value")
	})

	t.Run("contract without an action is rejected", func(t *testing.T) {
		bad := contract("no-action")
		bad.Spec.Action = ""
		mustFail(t, c.Create(ctx, bad), "spec.action")
	})

	t.Run("observation with an unknown status value is rejected", func(t *testing.T) {
		bad := observation("maybe")
		bad.Spec.Status = "Maybe"
		mustFail(t, c.Create(ctx, bad), "spec.status")
	})

	t.Run("action, requester and target cannot change", func(t *testing.T) {
		const want = "action, requester and target cannot change after the request is created"
		r := request("fixed")
		if err := c.Create(ctx, r); err != nil {
			t.Fatal(err)
		}
		for name, change := range map[string]func(*v1alpha1.ActionRequest){
			"target name":  func(r *v1alpha1.ActionRequest) { r.Spec.Target.Name = "b" },
			"target label": func(r *v1alpha1.ActionRequest) { r.Spec.Target.Labels["shard"] = "2" },
			"action":       func(r *v1alpha1.ActionRequest) { r.Spec.Action = "reboot" },
			"requester":    func(r *v1alpha1.ActionRequest) { r.Spec.Requester = "other" },
		} {
			changed := r.DeepCopy()
			change(changed)
			if err := c.Update(ctx, changed); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("%s: want %q, got %v", name, want, err)
			}
		}
	})

	// decide writes a decision the way the coordinator does.
	decide := func(t *testing.T, r *v1alpha1.ActionRequest, d v1alpha1.Decision) {
		t.Helper()
		now := metav1.NewMicroTime(time.Now().UTC().Truncate(time.Microsecond))
		r.Status = v1alpha1.ActionRequestStatus{Decision: d, DecidedAt: &now}
		if d == v1alpha1.DecisionGranted {
			deadline := metav1.NewMicroTime(now.Add(10 * time.Minute))
			r.Status.Deadline = &deadline
		}
		if err := c.Status().Update(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	const notGranted = "a result can be reported only for a granted request"

	t.Run("request cannot be created with a result", func(t *testing.T) {
		r := request("result-at-create")
		r.Spec.Result = &v1alpha1.Result{Outcome: v1alpha1.OutcomeSucceeded}
		mustFail(t, c.Create(ctx, r), notGranted)
	})

	t.Run("result cannot be reported before a decision or after a refusal", func(t *testing.T) {
		r := request("result-early")
		if err := c.Create(ctx, r); err != nil {
			t.Fatal(err)
		}
		r.Spec.Result = &v1alpha1.Result{Outcome: v1alpha1.OutcomeSucceeded}
		mustFail(t, c.Update(ctx, r), notGranted)

		r.Spec.Result = nil
		decide(t, r, v1alpha1.DecisionRefused)
		r.Spec.Result = &v1alpha1.Result{Outcome: v1alpha1.OutcomeSucceeded}
		mustFail(t, c.Update(ctx, r), notGranted)
	})

	t.Run("result is reported once and cannot change", func(t *testing.T) {
		const fixed = "the result cannot change after it is reported"
		r := request("result-once")
		if err := c.Create(ctx, r); err != nil {
			t.Fatal(err)
		}
		decide(t, r, v1alpha1.DecisionGranted)

		r.Spec.Result = &v1alpha1.Result{Outcome: "Other"}
		mustFail(t, c.Update(ctx, r), "spec.result.outcome")

		r.Spec.Result = &v1alpha1.Result{Outcome: v1alpha1.OutcomeFailed, Message: "did not come back"}
		if err := c.Update(ctx, r); err != nil {
			t.Fatal(err)
		}
		var out v1alpha1.ActionRequest
		if err := c.Get(ctx, client.ObjectKeyFromObject(r), &out); err != nil {
			t.Fatal(err)
		}
		if g, open := out.OpenGrant(); !open || !g.Failed {
			t.Fatalf("a failed request must keep its grant: open=%v %+v", open, g)
		}

		// A failure cannot be turned into a success, and cannot be removed.
		out.Spec.Result.Outcome = v1alpha1.OutcomeSucceeded
		mustFail(t, c.Update(ctx, &out), fixed)
		out.Spec.Result = nil
		mustFail(t, c.Update(ctx, &out), fixed)
	})

	t.Run("requester cannot write the decision", func(t *testing.T) {
		r := request("self-grant")
		r.Status.Decision = v1alpha1.DecisionGranted
		if err := c.Create(ctx, r); err != nil {
			t.Fatal(err)
		}
		var out v1alpha1.ActionRequest
		if err := c.Get(ctx, client.ObjectKeyFromObject(r), &out); err != nil {
			t.Fatal(err)
		}
		if out.Status.Decision != "" {
			t.Fatalf("a create or update of the object must not set the status, got %q", out.Status.Decision)
		}
	})

	t.Run("status keeps the grant and rejects an unknown decision", func(t *testing.T) {
		r := request("decided")
		if err := c.Create(ctx, r); err != nil {
			t.Fatal(err)
		}
		now := metav1.NewMicroTime(time.Now().UTC().Truncate(time.Microsecond))
		deadline := metav1.NewMicroTime(now.Add(10 * time.Minute))
		r.Status = v1alpha1.ActionRequestStatus{Decision: v1alpha1.DecisionGranted, DecidedAt: &now, Deadline: &deadline}
		if err := c.Status().Update(ctx, r); err != nil {
			t.Fatal(err)
		}
		var out v1alpha1.ActionRequest
		if err := c.Get(ctx, client.ObjectKeyFromObject(r), &out); err != nil {
			t.Fatal(err)
		}
		g, open := out.OpenGrant()
		if !open || !g.GrantedAt.Equal(now.Time) || !g.Deadline.Equal(deadline.Time) {
			t.Fatalf("grant did not survive the API server: open=%v %+v", open, g)
		}

		out.Status.Decision = "Maybe"
		mustFail(t, c.Status().Update(ctx, &out), "status.decision")
	})
}
