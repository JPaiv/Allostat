package controller_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/JPaiv/Allostat/api/v1alpha1"
	"github.com/JPaiv/Allostat/internal/controller"
)

// These tests run the reconciler against a real API server.
// They need the test binaries: run `make test`.

var (
	env    *envtest.Environment
	scheme = runtime.NewScheme()
	k8s    client.Client
	nsSeq  int
)

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Println("KUBEBUILDER_ASSETS is not set; run `make test`. Skipping.")
		os.Exit(0)
	}
	env = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		fmt.Println("start the API server:", err)
		os.Exit(1)
	}
	must(clientgoscheme.AddToScheme(scheme))
	must(v1alpha1.AddToScheme(scheme))
	k8s, err = client.New(cfg, client.Options{Scheme: scheme})
	must(err)

	code := m.Run()
	_ = env.Stop()
	os.Exit(code)
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

// world is one namespace with a reconciler whose clock the test controls.
type world struct {
	t   *testing.T
	ns  string
	now time.Time
	r   *controller.ActionRequestReconciler
}

func newWorld(t *testing.T) *world {
	t.Helper()
	nsSeq++
	w := &world{t: t, ns: fmt.Sprintf("test-%d", nsSeq), now: t0}
	w.r = &controller.ActionRequestReconciler{
		Reader: k8s, // the test client has no cache
		Status: k8s.Status(),
		Now:    func() time.Time { return w.now },
	}
	w.create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: w.ns}})
	return w
}

func (w *world) create(obj client.Object) {
	w.t.Helper()
	if err := k8s.Create(context.Background(), obj); err != nil {
		w.t.Fatalf("create %T %q: %v", obj, obj.GetName(), err)
	}
}

func (w *world) contract(name string) {
	w.t.Helper()
	w.create(&v1alpha1.Contract{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: w.ns},
		Spec: v1alpha1.ContractSpec{
			Action:   "restart",
			Requires: []v1alpha1.Requirement{{Condition: "PeerCanServe", MaxAge: metav1.Duration{Duration: time.Minute}}},
			Budgets:  []v1alpha1.Budget{{Label: "shard", Max: 1}},
			GrantTTL: metav1.Duration{Duration: 10 * time.Minute},
		},
	})
}

// observe reports PeerCanServe for a subject, measured at the world's time.
func (w *world) observe(subject string, status metav1.ConditionStatus) {
	w.t.Helper()
	w.create(&v1alpha1.Observation{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "obs-", Namespace: w.ns},
		Spec: v1alpha1.ObservationSpec{
			Condition: "PeerCanServe", Subject: subject, Status: status,
			Source: "replica-health", ObservedAt: metav1.NewMicroTime(w.now),
		},
	})
}

// ask creates a request for a target in a shard and reconciles it.
func (w *world) ask(name, target, shard string) v1alpha1.ActionRequestStatus {
	w.t.Helper()
	w.create(&v1alpha1.ActionRequest{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: w.ns},
		Spec: v1alpha1.ActionRequestSpec{
			Action: "restart", Requester: "upgrade",
			Target: v1alpha1.Target{Name: target, Labels: map[string]string{"shard": shard}},
		},
	})
	return w.reconcile(name)
}

func (w *world) reconcile(name string) v1alpha1.ActionRequestStatus {
	w.t.Helper()
	key := client.ObjectKey{Namespace: w.ns, Name: name}
	if _, err := w.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		w.t.Fatalf("reconcile %q: %v", name, err)
	}
	var out v1alpha1.ActionRequest
	if err := k8s.Get(context.Background(), key, &out); err != nil {
		w.t.Fatalf("get %q: %v", name, err)
	}
	return out.Status
}

func wantGranted(t *testing.T, s v1alpha1.ActionRequestStatus) {
	t.Helper()
	if s.Decision != v1alpha1.DecisionGranted || len(s.Reasons) != 0 || s.Deadline == nil {
		t.Fatalf("want a grant, got %+v", s)
	}
}

func wantRefused(t *testing.T, s v1alpha1.ActionRequestStatus, reason string) {
	t.Helper()
	if s.Decision != v1alpha1.DecisionRefused || s.Deadline != nil {
		t.Fatalf("want a refusal, got %+v", s)
	}
	if !strings.Contains(strings.Join(s.Reasons, "\n"), reason) {
		t.Fatalf("reasons %q do not contain %q", s.Reasons, reason)
	}
}

func TestGrantWithFreshEvidence(t *testing.T) {
	w := newWorld(t)
	w.contract("restart")
	w.observe("a", metav1.ConditionTrue)
	w.now = t0.Add(10 * time.Second)

	s := w.ask("r1", "a", "1")
	wantGranted(t, s)
	if !s.DecidedAt.Time.Equal(w.now) || !s.Deadline.Time.Equal(w.now.Add(10*time.Minute)) {
		t.Fatalf("wrong times: decided %v, deadline %v", s.DecidedAt, s.Deadline)
	}
}

func TestRefuseWithoutEvidence(t *testing.T) {
	w := newWorld(t)
	w.contract("restart")
	wantRefused(t, w.ask("r1", "a", "1"), `no observation of "PeerCanServe" for "a"`)
}

func TestRefuseWithOldOrBadEvidence(t *testing.T) {
	w := newWorld(t)
	w.contract("restart")
	w.observe("a", metav1.ConditionTrue)
	w.now = t0.Add(2 * time.Minute)
	w.observe("b", metav1.ConditionFalse)

	wantRefused(t, w.ask("r1", "a", "1"), "2m0s old, limit is 1m0s")
	wantRefused(t, w.ask("r2", "b", "2"), `"PeerCanServe" for "b" is False`)
}

func TestRefuseWithoutExactlyOneContract(t *testing.T) {
	w := newWorld(t)
	w.observe("a", metav1.ConditionTrue)
	wantRefused(t, w.ask("r1", "a", "1"), `no contract covers action "restart"`)

	w.contract("one")
	w.contract("two")
	wantRefused(t, w.ask("r2", "a", "1"), `2 contracts cover action "restart"`)
}

func TestBudgetCountsGrantsWrittenBefore(t *testing.T) {
	w := newWorld(t)
	w.contract("restart")
	w.observe("a", metav1.ConditionTrue)
	w.observe("b", metav1.ConditionTrue)
	w.observe("c", metav1.ConditionTrue)

	wantGranted(t, w.ask("r1", "a", "1"))
	// b is in the same shard as a: the grant of r1 must block it.
	wantRefused(t, w.ask("r2", "b", "1"), "budget shard=1 is full: 1 of 1 in use")
	// c is in another shard.
	wantGranted(t, w.ask("r3", "c", "2"))
}

func TestOverdueGrantStillBlocks(t *testing.T) {
	w := newWorld(t)
	w.contract("restart")
	w.observe("a", metav1.ConditionTrue)
	wantGranted(t, w.ask("r1", "a", "1"))

	w.now = t0.Add(time.Hour)
	w.observe("b", metav1.ConditionTrue)
	wantRefused(t, w.ask("r2", "b", "1"), "1 of 1 in use, 1 overdue")
}

func TestDeletedRequestFreesTheBudget(t *testing.T) {
	w := newWorld(t)
	w.contract("restart")
	w.observe("a", metav1.ConditionTrue)
	w.observe("b", metav1.ConditionTrue)
	wantGranted(t, w.ask("r1", "a", "1"))

	r1 := &v1alpha1.ActionRequest{ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: w.ns}}
	if err := k8s.Delete(context.Background(), r1); err != nil {
		t.Fatal(err)
	}
	wantGranted(t, w.ask("r2", "b", "1"))
}

func TestDecisionIsFinal(t *testing.T) {
	w := newWorld(t)
	w.contract("restart")

	refused := w.ask("r1", "a", "1")
	wantRefused(t, refused, "no observation")
	// Evidence arrives later. The old request must stay refused.
	w.observe("a", metav1.ConditionTrue)
	w.now = t0.Add(time.Second)
	again := w.reconcile("r1")
	wantRefused(t, again, "no observation")
	if !again.DecidedAt.Equal(refused.DecidedAt) {
		t.Fatalf("the request was decided again: %v then %v", refused.DecidedAt, again.DecidedAt)
	}

	granted := w.ask("r2", "a", "1")
	wantGranted(t, granted)
	// The evidence is old an hour later. The grant must stay as it was.
	w.now = t0.Add(time.Hour)
	later := w.reconcile("r2")
	wantGranted(t, later)
	if !later.DecidedAt.Equal(granted.DecidedAt) || !later.Deadline.Equal(granted.Deadline) {
		t.Fatalf("the grant changed: %+v then %+v", granted, later)
	}
}

func TestNamespacesAreSeparate(t *testing.T) {
	one, two := newWorld(t), newWorld(t)
	one.contract("restart")
	one.observe("a", metav1.ConditionTrue)
	two.observe("a", metav1.ConditionTrue)
	two.observe("b", metav1.ConditionTrue)

	wantGranted(t, one.ask("r1", "a", "1"))
	// The contract of the first namespace does not cover the second.
	wantRefused(t, two.ask("r1", "a", "1"), "no contract covers action")
	// A grant in the first namespace does not use the budget of the second.
	two.contract("restart")
	wantGranted(t, two.ask("r2", "b", "1"))
}

func TestMissingRequestIsNotAnError(t *testing.T) {
	w := newWorld(t)
	key := client.ObjectKey{Namespace: w.ns, Name: "gone"}
	if _, err := w.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("want no error, got %v", err)
	}
}

// The reconciler must also work when the manager runs it: the watch starts
// it and it reads from the API server.
func TestManagerDecidesNewRequests(t *testing.T) {
	w := newWorld(t)
	w.contract("restart")
	w.now = time.Now()
	w.observe("a", metav1.ConditionTrue)
	w.observe("b", metav1.ConditionTrue)

	mgr, err := ctrl.NewManager(env.Config, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		// The test may run more than once in one process.
		Controller: config.Controller{SkipNameValidation: new(true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := &controller.ActionRequestReconciler{Reader: mgr.GetAPIReader(), Status: mgr.GetClient().Status(), Now: time.Now}
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("manager: %v", err)
		}
	})

	for _, target := range []string{"a", "b"} {
		w.create(&v1alpha1.ActionRequest{
			ObjectMeta: metav1.ObjectMeta{Name: "m-" + target, Namespace: w.ns},
			Spec: v1alpha1.ActionRequestSpec{
				Action: "restart", Requester: "upgrade",
				Target: v1alpha1.Target{Name: target, Labels: map[string]string{"shard": "1"}},
			},
		})
	}

	// Both targets are in one shard with a budget of one. Whatever the
	// order, exactly one request is granted.
	deadline := time.Now().Add(20 * time.Second)
	for {
		var list v1alpha1.ActionRequestList
		if err := k8s.List(context.Background(), &list, client.InNamespace(w.ns)); err != nil {
			t.Fatal(err)
		}
		granted, refused := 0, 0
		for _, item := range list.Items {
			switch item.Status.Decision {
			case v1alpha1.DecisionGranted:
				granted++
			case v1alpha1.DecisionRefused:
				refused++
			}
		}
		if granted+refused == 2 {
			if granted != 1 {
				t.Fatalf("want exactly one grant, got %d granted and %d refused", granted, refused)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("requests were not decided in time: %d granted, %d refused", granted, refused)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
