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

	t.Run("request spec cannot change", func(t *testing.T) {
		r := request("fixed")
		if err := c.Create(ctx, r); err != nil {
			t.Fatal(err)
		}
		r.Spec.Target.Name = "b"
		mustFail(t, c.Update(ctx, r), "spec cannot change after the request is created")
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
