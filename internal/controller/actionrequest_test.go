package controller_test

import (
	"context"
	"errors"
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
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
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
	k8s    client.WithWatch
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
	k8s, err = client.NewWithWatch(cfg, client.Options{Scheme: scheme})
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
		Writer: k8s,
		Now:    func() time.Time { return w.now },
	}
	w.create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: w.ns}})
	// Whatever a test does, every decision it leaves behind must have a
	// record that gives the same answer when it is played again.
	t.Cleanup(func() {
		if !t.Failed() {
			w.checkRecords()
		}
	})
	return w
}

// checkRecords looks at every decided request of the world: its status
// must name a record, the record must be about this request and say what
// the status says, and a replay of the record must give that answer.
func (w *world) checkRecords() int {
	w.t.Helper()
	ctx := context.Background()
	var requests v1alpha1.ActionRequestList
	if err := k8s.List(ctx, &requests, client.InNamespace(w.ns)); err != nil {
		w.t.Fatal(err)
	}
	checked := 0
	for i := range requests.Items {
		ar := &requests.Items[i]
		s := ar.Status
		if s.Decision == "" {
			continue
		}
		checked++
		if s.Record == "" {
			w.t.Errorf("request %q has a decision without a record", ar.Name)
			continue
		}
		var rec v1alpha1.DecisionRecord
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: w.ns, Name: s.Record}, &rec); err != nil {
			w.t.Errorf("request %q: record %q: %v", ar.Name, s.Record, err)
			continue
		}
		rs := rec.Spec
		if rs.Request.Name != ar.Name || rs.Request.UID != ar.UID {
			w.t.Errorf("record %q is about %q (%s), not %q (%s)", rec.Name, rs.Request.Name, rs.Request.UID, ar.Name, ar.UID)
		}
		if len(rec.OwnerReferences) != 1 || rec.OwnerReferences[0].UID != ar.UID {
			w.t.Errorf("record %q is not owned by its request: %+v", rec.Name, rec.OwnerReferences)
		}
		if rs.Decision.Decision != s.Decision || strings.Join(rs.Decision.Reasons, "|") != strings.Join(s.Reasons, "|") {
			w.t.Errorf("record %q says %s %q, the status says %s %q", rec.Name, rs.Decision.Decision, rs.Decision.Reasons, s.Decision, s.Reasons)
		}
		if !rs.Time.Equal(s.DecidedAt) {
			w.t.Errorf("record %q has the time %s, the status has %s", rec.Name, rs.Time, s.DecidedAt)
		}
		if (rs.Decision.Deadline == nil) != (s.Deadline == nil) || (s.Deadline != nil && !rs.Decision.Deadline.Equal(s.Deadline)) {
			w.t.Errorf("record %q has the deadline %v, the status has %v", rec.Name, rs.Decision.Deadline, s.Deadline)
		}
		if err := rec.Verify(); err != nil {
			w.t.Errorf("record %q does not replay: %v", rec.Name, err)
		}
		if s.Released != nil {
			w.checkRelease(ar)
		}
	}
	return checked
}

// checkRelease looks at a released request: the release must name a
// record, the record must be about this request and this release, and a
// replay of it must release the grant by the same evidence.
func (w *world) checkRelease(ar *v1alpha1.ActionRequest) {
	w.t.Helper()
	released := ar.Status.Released
	if released.Record == "" {
		w.t.Errorf("request %q has a release without a record", ar.Name)
		return
	}
	var rec v1alpha1.ReleaseRecord
	if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: w.ns, Name: released.Record}, &rec); err != nil {
		w.t.Errorf("request %q: release record %q: %v", ar.Name, released.Record, err)
		return
	}
	rs := rec.Spec
	if rs.Request.Name != ar.Name || rs.Request.UID != ar.UID {
		w.t.Errorf("release record %q is about %q (%s), not %q (%s)", rec.Name, rs.Request.Name, rs.Request.UID, ar.Name, ar.UID)
	}
	if len(rec.OwnerReferences) != 1 || rec.OwnerReferences[0].UID != ar.UID {
		w.t.Errorf("release record %q is not owned by its request: %+v", rec.Name, rec.OwnerReferences)
	}
	if !rs.Time.Equal(&released.At) {
		w.t.Errorf("release record %q has the time %s, the status has %s", rec.Name, rs.Time, released.At)
	}
	if !rs.Grant.GrantedAt.Equal(ar.Status.DecidedAt) || !rs.Grant.Deadline.Equal(ar.Status.Deadline) {
		w.t.Errorf("release record %q has another grant than the status: %+v", rec.Name, rs.Grant)
	}
	if len(rs.Evidence) != len(released.Evidence) {
		w.t.Errorf("release record %q has %d observations as evidence, the status has %d", rec.Name, len(rs.Evidence), len(released.Evidence))
	}
	if err := rec.Verify(); err != nil {
		w.t.Errorf("release record %q does not replay: %v", rec.Name, err)
	}
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
	w.observeCondition("PeerCanServe", subject, status)
}

func (w *world) observeCondition(condition, subject string, status metav1.ConditionStatus) {
	w.t.Helper()
	w.create(&v1alpha1.Observation{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "obs-", Namespace: w.ns},
		Spec: v1alpha1.ObservationSpec{
			Condition: condition, Subject: subject, Status: status,
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

func TestClockSkewOfTheContractIsUsed(t *testing.T) {
	w := newWorld(t)
	w.create(&v1alpha1.Contract{
		ObjectMeta: metav1.ObjectMeta{Name: "restart", Namespace: w.ns},
		Spec: v1alpha1.ContractSpec{
			Action:    "restart",
			Requires:  []v1alpha1.Requirement{{Condition: "PeerCanServe", MaxAge: metav1.Duration{Duration: time.Minute}}},
			GrantTTL:  metav1.Duration{Duration: 10 * time.Minute},
			ClockSkew: &metav1.Duration{Duration: 2 * time.Second},
		},
	})
	// The reporter's clock is ahead of the coordinator's clock.
	w.now = t0.Add(time.Second)
	w.observe("a", metav1.ConditionTrue)
	w.now = t0.Add(5 * time.Second)
	w.observe("b", metav1.ConditionTrue)
	w.now = t0

	wantGranted(t, w.ask("r1", "a", "1"))
	wantRefused(t, w.ask("r2", "b", "2"), "from the future (5s ahead, limit is 2s)")
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

// report writes the requester's result into a granted request.
func (w *world) report(name string, outcome v1alpha1.Outcome) {
	w.t.Helper()
	var r v1alpha1.ActionRequest
	if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: w.ns, Name: name}, &r); err != nil {
		w.t.Fatal(err)
	}
	r.Spec.Result = &v1alpha1.Result{Outcome: outcome}
	if err := k8s.Update(context.Background(), &r); err != nil {
		w.t.Fatalf("report %q for %q: %v", outcome, name, err)
	}
}

func TestSuccessFreesTheBudget(t *testing.T) {
	w := newWorld(t)
	w.contract("restart")
	w.observe("a", metav1.ConditionTrue)
	w.observe("b", metav1.ConditionTrue)
	wantGranted(t, w.ask("r1", "a", "1"))
	wantRefused(t, w.ask("r2", "b", "1"), "budget shard=1 is full")

	w.report("r1", v1alpha1.OutcomeSucceeded)
	wantGranted(t, w.ask("r3", "b", "1"))
	// The report does not change the decision that was made.
	wantGranted(t, w.reconcile("r1"))
}

func TestFailureKeepsTheBudget(t *testing.T) {
	w := newWorld(t)
	w.contract("restart")
	w.observe("a", metav1.ConditionTrue)
	w.observe("b", metav1.ConditionTrue)
	wantGranted(t, w.ask("r1", "a", "1"))

	w.report("r1", v1alpha1.OutcomeFailed)
	wantRefused(t, w.ask("r2", "b", "1"), "1 of 1 in use, 0 overdue, 1 failed")
	wantRefused(t, w.ask("r3", "a", "1"), `already has an open grant (request "r1"), reported as failed`)

	// A person clears a failed grant by deleting the request.
	r1 := &v1alpha1.ActionRequest{ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: w.ns}}
	if err := k8s.Delete(context.Background(), r1); err != nil {
		t.Fatal(err)
	}
	wantGranted(t, w.ask("r4", "b", "1"))
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

	startCoordinator(t)

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

// startCoordinator runs the reconciler under a manager, without leader
// election, until the test ends.
func startCoordinator(t *testing.T) {
	t.Helper()
	mgr, err := ctrl.NewManager(env.Config, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		// Several coordinators run in one test process.
		Controller: config.Controller{SkipNameValidation: new(true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := &controller.ActionRequestReconciler{Reader: mgr.GetAPIReader(), Writer: mgr.GetClient(), Now: time.Now}
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
}

// request creates a request and does not reconcile it.
func (w *world) request(name, target, shard string) {
	w.t.Helper()
	w.create(&v1alpha1.ActionRequest{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: w.ns},
		Spec: v1alpha1.ActionRequestSpec{
			Action: "restart", Requester: "upgrade",
			Target: v1alpha1.Target{Name: target, Labels: map[string]string{"shard": shard}},
		},
	})
}

func (w *world) status(name string) v1alpha1.ActionRequestStatus {
	w.t.Helper()
	var out v1alpha1.ActionRequest
	if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: w.ns, Name: name}, &out); err != nil {
		w.t.Fatal(err)
	}
	return out.Status
}

// ledgerRequests returns the request names in the ledger.
func (w *world) ledgerRequests() []string {
	w.t.Helper()
	var ledger v1alpha1.Ledger
	key := client.ObjectKey{Namespace: w.ns, Name: v1alpha1.LedgerName}
	if err := k8s.Get(context.Background(), key, &ledger); err != nil {
		w.t.Fatal(err)
	}
	var names []string
	for _, g := range ledger.Status.Grants {
		names = append(names, g.Request)
	}
	return names
}

// withStatusHook returns a reconciler for the world whose status writes go
// through hook first. If hook returns an error, the write does not happen.
func (w *world) withStatusHook(hook func(obj client.Object) error) *controller.ActionRequestReconciler {
	writer := interceptor.NewClient(k8s, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if err := hook(obj); err != nil {
				return err
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	return &controller.ActionRequestReconciler{Reader: k8s, Writer: writer, Now: func() time.Time { return w.now }}
}

// Two coordinators decide from the same state. The one that writes the
// ledger second must lose and decide again.
func TestSecondLedgerWriterDecidesAgain(t *testing.T) {
	w := newWorld(t)
	w.contract("restart")
	w.observe("a", metav1.ConditionTrue)
	w.observe("b", metav1.ConditionTrue)
	w.request("r1", "a", "1")
	w.request("r2", "b", "1")

	// The slow coordinator has decided to grant r1. Before its ledger
	// write arrives, the other coordinator grants r2 completely.
	interrupted := false
	slow := w.withStatusHook(func(obj client.Object) error {
		if _, isLedger := obj.(*v1alpha1.Ledger); isLedger && !interrupted {
			interrupted = true
			wantGranted(t, w.reconcile("r2"))
		}
		return nil
	})
	key := client.ObjectKey{Namespace: w.ns, Name: "r1"}
	res, err := slow.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil || res.RequeueAfter == 0 {
		t.Fatalf("want a retry without an error, got %+v, %v", res, err)
	}
	if !interrupted {
		t.Fatal("the slow coordinator never wrote the ledger")
	}
	if s := w.status("r1"); s.Decision != "" {
		t.Fatalf("r1 must not be decided by the lost write, got %+v", s)
	}

	if _, err := slow.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	wantRefused(t, w.status("r1"), "budget shard=1 is full")
}

// The coordinator stops after the ledger write and before the status
// write. The grant must count at once and be finished later, unchanged.
func TestGrantIsFinishedFromTheLedger(t *testing.T) {
	w := newWorld(t)
	w.contract("restart")
	w.observe("a", metav1.ConditionTrue)
	w.observe("b", metav1.ConditionTrue)
	w.request("r1", "a", "1")

	stopped := errors.New("stopped")
	crashing := w.withStatusHook(func(obj client.Object) error {
		if _, isRequest := obj.(*v1alpha1.ActionRequest); isRequest {
			return stopped
		}
		return nil
	})
	key := client.ObjectKey{Namespace: w.ns, Name: "r1"}
	if _, err := crashing.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); !errors.Is(err, stopped) {
		t.Fatalf("want the injected error, got %v", err)
	}
	if s := w.status("r1"); s.Decision != "" {
		t.Fatalf("r1 has a status although the write was stopped: %+v", s)
	}

	// The grant is only in the ledger, and it already holds the budget.
	wantRefused(t, w.ask("r2", "b", "1"), "budget shard=1 is full")

	// Much later the coordinator runs again. The grant keeps its times,
	// and the evidence is not judged again.
	w.now = t0.Add(time.Hour)
	s := w.reconcile("r1")
	wantGranted(t, s)
	if !s.DecidedAt.Time.Equal(t0) || !s.Deadline.Time.Equal(t0.Add(10*time.Minute)) {
		t.Fatalf("the grant changed: decided %v, deadline %v", s.DecidedAt, s.Deadline)
	}
}

func TestLostLedgerDoesNotFreeTheBudget(t *testing.T) {
	w := newWorld(t)
	w.contract("restart")
	w.observe("a", metav1.ConditionTrue)
	w.observe("b", metav1.ConditionTrue)
	w.observe("c", metav1.ConditionTrue)
	wantGranted(t, w.ask("r1", "a", "1"))

	ledger := &v1alpha1.Ledger{ObjectMeta: metav1.ObjectMeta{Namespace: w.ns, Name: v1alpha1.LedgerName}}
	if err := k8s.Delete(context.Background(), ledger); err != nil {
		t.Fatal(err)
	}
	wantRefused(t, w.ask("r2", "b", "1"), "budget shard=1 is full")
	// The next grant puts the old one back into the new ledger.
	wantGranted(t, w.ask("r3", "c", "2"))
	if got := strings.Join(w.ledgerRequests(), ","); got != "r1,r3" {
		t.Fatalf("ledger has %q, want r1,r3", got)
	}
}

func TestLedgerDropsClosedGrants(t *testing.T) {
	w := newWorld(t)
	w.contract("restart")
	w.observe("a", metav1.ConditionTrue)
	w.observe("b", metav1.ConditionTrue)
	w.observe("c", metav1.ConditionTrue)
	wantGranted(t, w.ask("r1", "a", "1"))
	wantGranted(t, w.ask("r2", "b", "2"))
	w.report("r1", v1alpha1.OutcomeSucceeded)
	if err := k8s.Delete(context.Background(), &v1alpha1.ActionRequest{ObjectMeta: metav1.ObjectMeta{Name: "r2", Namespace: w.ns}}); err != nil {
		t.Fatal(err)
	}

	wantGranted(t, w.ask("r3", "c", "3"))
	if got := strings.Join(w.ledgerRequests(), ","); got != "r3" {
		t.Fatalf("ledger has %q, want only r3", got)
	}
}

// A new request with the name of a deleted one must not inherit its grant.
func TestSameNameDoesNotInheritAGrant(t *testing.T) {
	w := newWorld(t)
	w.contract("restart")
	w.observe("a", metav1.ConditionTrue)
	wantGranted(t, w.ask("r1", "a", "1"))
	if err := k8s.Delete(context.Background(), &v1alpha1.ActionRequest{ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: w.ns}}); err != nil {
		t.Fatal(err)
	}

	// The evidence is old now, so the new r1 must be judged and refused.
	w.now = t0.Add(time.Hour)
	wantRefused(t, w.ask("r1", "a", "1"), "old, limit is 1m0s")
}

// Two coordinators run without leader election against the same requests.
// No budget may be exceeded.
func TestTwoCoordinatorsNeverExceedABudget(t *testing.T) {
	w := newWorld(t)
	w.contract("restart")
	w.now = time.Now()
	const shards, perShard = 4, 8
	for s := 0; s < shards; s++ {
		for n := 0; n < perShard; n++ {
			w.observe(fmt.Sprintf("node-%d-%d", s, n), metav1.ConditionTrue)
		}
	}

	startCoordinator(t)
	startCoordinator(t)

	for s := 0; s < shards; s++ {
		for n := 0; n < perShard; n++ {
			target := fmt.Sprintf("node-%d-%d", s, n)
			w.request("req-"+target, target, fmt.Sprint(s))
		}
	}

	deadline := time.Now().Add(60 * time.Second)
	for {
		var list v1alpha1.ActionRequestList
		if err := k8s.List(context.Background(), &list, client.InNamespace(w.ns)); err != nil {
			t.Fatal(err)
		}
		decided := 0
		granted := map[string]int{}
		for _, item := range list.Items {
			if item.Status.Decision != "" {
				decided++
			}
			if item.Status.Decision == v1alpha1.DecisionGranted {
				granted[item.Spec.Target.Labels["shard"]]++
			}
		}
		for shard, n := range granted {
			if n > 1 {
				t.Fatalf("shard %s has %d grants, budget is 1", shard, n)
			}
		}
		if decided == shards*perShard {
			if len(granted) != shards {
				t.Fatalf("want one grant in each of %d shards, got %v", shards, granted)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d requests were decided in time", decided, shards*perShard)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// releaseContract is the restart contract with a way out of an overdue or
// failed grant: the target must be seen serving.
func (w *world) releaseContract() {
	w.t.Helper()
	w.create(&v1alpha1.Contract{
		ObjectMeta: metav1.ObjectMeta{Name: "restart", Namespace: w.ns},
		Spec: v1alpha1.ContractSpec{
			Action:      "restart",
			Requires:    []v1alpha1.Requirement{{Condition: "PeerCanServe", MaxAge: metav1.Duration{Duration: time.Minute}}},
			Budgets:     []v1alpha1.Budget{{Label: "shard", Max: 1}},
			GrantTTL:    metav1.Duration{Duration: 10 * time.Minute},
			ReleaseWhen: []v1alpha1.Requirement{{Condition: "TargetServing", MaxAge: metav1.Duration{Duration: time.Minute}}},
		},
	})
}

func TestEvidenceReleasesAnOverdueGrant(t *testing.T) {
	w := newWorld(t)
	w.releaseContract()
	w.observe("a", metav1.ConditionTrue)
	// The target was serving before the action. That must not count later.
	w.observeCondition("TargetServing", "a", metav1.ConditionTrue)
	wantGranted(t, w.ask("r1", "a", "1"))

	// Before the deadline nothing is released.
	w.now = t0.Add(9*time.Minute + 50*time.Second)
	w.observeCondition("TargetServing", "a", metav1.ConditionTrue)
	if s := w.reconcile("r1"); s.Released != nil {
		t.Fatalf("released before the deadline: %+v", s)
	}

	// Overdue, and the newest evidence is fresh, but it is from before the
	// deadline.
	w.now = t0.Add(10*time.Minute + 10*time.Second)
	if s := w.reconcile("r1"); s.Released != nil {
		t.Fatalf("released with evidence from before the deadline: %+v", s)
	}
	w.observe("b", metav1.ConditionTrue)
	wantRefused(t, w.ask("r2", "b", "1"), "1 of 1 in use, 1 overdue")

	// Evidence from after the deadline closes the grant.
	w.observeCondition("TargetServing", "a", metav1.ConditionTrue)
	s := w.reconcile("r1")
	wantGranted(t, s)
	if s.Released == nil || !s.Released.At.Time.Equal(w.now) || len(s.Released.Evidence) != 1 ||
		!strings.Contains(s.Released.Evidence[0], `TargetServing=True for "a" from "replica-health"`) {
		t.Fatalf("want a release with its evidence, got %+v", s.Released)
	}
	wantGranted(t, w.ask("r3", "b", "1"))
	if got := strings.Join(w.ledgerRequests(), ","); got != "r3" {
		t.Fatalf("ledger has %q, want only r3", got)
	}
}

func TestEvidenceReleasesAFailedGrant(t *testing.T) {
	w := newWorld(t)
	w.releaseContract()
	w.observe("a", metav1.ConditionTrue)
	wantGranted(t, w.ask("r1", "a", "1"))

	// The target is seen serving, then the requester reports a failure.
	w.now = t0.Add(time.Minute)
	w.observeCondition("TargetServing", "a", metav1.ConditionTrue)
	w.report("r1", v1alpha1.OutcomeFailed)

	// The coordinator sees the report later. Evidence from before that
	// moment does not count.
	w.now = t0.Add(time.Minute + 10*time.Second)
	s := w.reconcile("r1")
	if s.FailureSeenAt == nil || !s.FailureSeenAt.Time.Equal(w.now) || s.Released != nil {
		t.Fatalf("want only the failure time, got %+v", s)
	}
	if s = w.reconcile("r1"); s.Released != nil {
		t.Fatalf("released with evidence from before the failure report: %+v", s)
	}

	w.now = t0.Add(time.Minute + 20*time.Second)
	w.observeCondition("TargetServing", "a", metav1.ConditionFalse)
	if s = w.reconcile("r1"); s.Released != nil {
		t.Fatalf("released while the target does not serve: %+v", s)
	}

	w.now = t0.Add(time.Minute + 30*time.Second)
	w.observeCondition("TargetServing", "a", metav1.ConditionTrue)
	s = w.reconcile("r1")
	if s.Released == nil || !s.FailureSeenAt.Time.Equal(t0.Add(time.Minute+10*time.Second)) {
		t.Fatalf("want a release and the first failure time, got %+v", s)
	}
	w.observe("a", metav1.ConditionTrue)
	wantGranted(t, w.ask("r2", "a", "1"))
}

// Without release conditions in the contract, evidence closes nothing.
func TestNoReleaseWithoutReleaseConditions(t *testing.T) {
	w := newWorld(t)
	w.contract("restart")
	w.observe("a", metav1.ConditionTrue)
	wantGranted(t, w.ask("r1", "a", "1"))

	w.now = t0.Add(time.Hour)
	w.observeCondition("TargetServing", "a", metav1.ConditionTrue)
	if s := w.reconcile("r1"); s.Released != nil {
		t.Fatalf("released without release conditions: %+v", s)
	}
	w.report("r1", v1alpha1.OutcomeFailed)
	w.reconcile("r1")
	w.now = t0.Add(2 * time.Hour)
	w.observeCondition("TargetServing", "a", metav1.ConditionTrue)
	if s := w.reconcile("r1"); s.Released != nil {
		t.Fatalf("released without release conditions: %+v", s)
	}
	w.observe("b", metav1.ConditionTrue)
	wantRefused(t, w.ask("r2", "b", "1"), "1 of 1 in use, 0 overdue, 1 failed")
}

// If the contract is gone or doubled, no rules are known and nothing is
// released.
func TestNoReleaseWithoutExactlyOneContract(t *testing.T) {
	w := newWorld(t)
	w.releaseContract()
	w.observe("a", metav1.ConditionTrue)
	wantGranted(t, w.ask("r1", "a", "1"))

	w.contract("restart-copy")
	w.now = t0.Add(time.Hour)
	w.observeCondition("TargetServing", "a", metav1.ConditionTrue)
	if s := w.reconcile("r1"); s.Released != nil {
		t.Fatalf("released with two contracts: %+v", s)
	}
}

// A success report that arrives late does not change the record of a
// release that was already written.
func TestLateSuccessAfterARelease(t *testing.T) {
	w := newWorld(t)
	w.releaseContract()
	w.observe("a", metav1.ConditionTrue)
	wantGranted(t, w.ask("r1", "a", "1"))

	w.now = t0.Add(11 * time.Minute)
	w.observeCondition("TargetServing", "a", metav1.ConditionTrue)
	if s := w.reconcile("r1"); s.Released == nil {
		t.Fatalf("want a release, got %+v", s)
	}
	w.report("r1", v1alpha1.OutcomeSucceeded)
	if s := w.reconcile("r1"); s.Released == nil || s.FailureSeenAt != nil {
		t.Fatalf("the record of the release changed: %+v", s)
	}
}

// The tests below are about a coordinator that was away. A coordinator
// that is down is a reconciler that is not called.

// A request that waited is decided with the evidence of the moment of the
// decision, not of the moment it was asked.
func TestWaitingRequestIsDecidedWithCurrentEvidence(t *testing.T) {
	w := newWorld(t)
	w.contract("restart")
	w.observe("a", metav1.ConditionTrue)
	w.request("r1", "a", "1")
	if s := w.status("r1"); s.Decision != "" {
		t.Fatalf("a request has an answer without a coordinator: %+v", s)
	}

	// The coordinator comes back an hour later.
	w.now = t0.Add(time.Hour)
	wantRefused(t, w.reconcile("r1"), `observation of "PeerCanServe" for "a" is 1h0m0s old`)
}

// requestUntil creates a request whose requester waits until a time. It
// does not reconcile it.
func (w *world) requestUntil(name, target, shard string, expiresAt time.Time) {
	w.t.Helper()
	at := metav1.NewTime(expiresAt)
	w.create(&v1alpha1.ActionRequest{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: w.ns},
		Spec: v1alpha1.ActionRequestSpec{
			Action: "restart", Requester: "upgrade",
			Target:    v1alpha1.Target{Name: target, Labels: map[string]string{"shard": shard}},
			ExpiresAt: &at,
		},
	})
}

// The requester gave up while the coordinator was away. All evidence is
// fresh when the coordinator comes back, but a grant would be for nobody.
func TestRequestThatExpiredWhileWaitingIsRefused(t *testing.T) {
	w := newWorld(t)
	w.contract("restart")
	w.requestUntil("r1", "a", "1", t0.Add(30*time.Second))
	w.requestUntil("r2", "b", "2", t0.Add(10*time.Minute))

	w.now = t0.Add(5 * time.Minute)
	w.observe("a", metav1.ConditionTrue)
	w.observe("b", metav1.ConditionTrue)
	wantRefused(t, w.reconcile("r1"), "request expired at 2026-01-01T12:00:30Z, before it was decided")
	wantGranted(t, w.reconcile("r2"))

	// The refusal holds no budget.
	wantGranted(t, w.ask("r3", "a", "1"))
}

// A requester without an expiry that gives up deletes its request. The
// coordinator then has nothing to grant.
func TestWithdrawnRequestGetsNoGrant(t *testing.T) {
	w := newWorld(t)
	w.contract("restart")
	w.observe("a", metav1.ConditionTrue)
	w.observe("b", metav1.ConditionTrue)
	w.request("r1", "a", "1")
	if err := k8s.Delete(context.Background(), &v1alpha1.ActionRequest{ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: w.ns}}); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKey{Namespace: w.ns, Name: "r1"}
	if _, err := w.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	wantGranted(t, w.ask("r2", "b", "1"))
}

// An action that started goes on without the coordinator. Its report can
// be written while the coordinator is away, also after the deadline, and
// it closes the grant.
func TestReportWithoutCoordinatorClosesTheGrant(t *testing.T) {
	w := newWorld(t)
	w.contract("restart")
	w.observe("a", metav1.ConditionTrue)
	wantGranted(t, w.ask("r1", "a", "1"))

	// No reconcile from here on: the coordinator is away.
	w.now = t0.Add(time.Hour)
	w.report("r1", v1alpha1.OutcomeSucceeded)

	// It comes back and the budget is free.
	w.observe("b", metav1.ConditionTrue)
	wantGranted(t, w.ask("r2", "b", "1"))
}

// A grant that became overdue while the coordinator was away still blocks
// when it comes back.
func TestGrantStaysOpenWhileTheCoordinatorIsAway(t *testing.T) {
	w := newWorld(t)
	w.releaseContract()
	w.observe("a", metav1.ConditionTrue)
	wantGranted(t, w.ask("r1", "a", "1"))
	// Seen serving before the deadline. No report.
	w.now = t0.Add(9*time.Minute + 30*time.Second)
	w.observeCondition("TargetServing", "a", metav1.ConditionTrue)

	w.now = t0.Add(10*time.Minute + 10*time.Second)
	w.observe("b", metav1.ConditionTrue)
	wantRefused(t, w.ask("r2", "b", "1"), "1 of 1 in use, 1 overdue")
	if s := w.reconcile("r1"); s.Released != nil {
		t.Fatalf("released with evidence from before the deadline: %+v", s)
	}
}
