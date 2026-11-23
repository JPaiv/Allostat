// Command rolling-restart shows Allostat on a fleet that does not exist.
//
// It starts a local API server, installs the Allostat resource types and
// runs three independent parts against it:
//
//   - the coordinator, the same code as cmd/allostat,
//   - a health reporter that writes what it sees as observations,
//   - an upgrade controller with one worker for each node. Every worker
//     asks before it restarts its node and reports the result after.
//
// All workers start at the same time. The contract allows one node of a
// shard to be out, so the coordinator lets one node of each shard go and
// refuses the others until the first is back.
//
// One worker stops before it reports. Its grant becomes overdue and blocks
// the shard, until the health reporter sees the node serving after the
// deadline and the coordinator closes the grant.
//
// The fleet is a map in memory. The example checks that two nodes of one
// shard were never out at the same time, and fails if they were.
//
// Run it with `make example`.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/JPaiv/Allostat/api/v1alpha1"
	"github.com/JPaiv/Allostat/internal/controller"
)

const (
	namespace   = "fleet"
	newVersion  = "v2"
	restartTime = 1500 * time.Millisecond
	// answerWait is how long a worker waits for an answer.
	answerWait = 10 * time.Second
	grantTTL   = 6 * time.Second
	// lostReport is the node whose worker stops before it reports.
	lostReport = "n2a"
	timeout    = 90 * time.Second
)

// started is the moment the example begins to talk.
var started = time.Now()

// say prints one line with the time since the start and who speaks.
func say(who, format string, args ...any) {
	fmt.Printf("%6.1fs  %-12s %s\n", time.Since(started).Seconds(), who, fmt.Sprintf(format, args...))
}

// node is one machine of the fleet.
type node struct {
	name, shard, version string
	up                   bool
}

// fleet is the thing that is upgraded. Only the worker of a node changes
// that node, and only the health reporter reads it.
type fleet struct {
	mu         sync.Mutex
	nodes      []*node
	violations []string
}

func newFleet() *fleet {
	f := &fleet{}
	for _, shard := range []string{"1", "2", "3"} {
		for _, replica := range []string{"a", "b"} {
			f.nodes = append(f.nodes, &node{name: "n" + shard + replica, shard: shard, version: "v1", up: true})
		}
	}
	return f
}

// stop takes a node out. It records a violation if another node of the
// same shard is out at that moment: then the shard cannot serve.
func (f *fleet) stop(n *node) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, other := range f.nodes {
		if other != n && other.shard == n.shard && !other.up {
			f.violations = append(f.violations, fmt.Sprintf("%s and %s of shard %s were out at the same time", other.name, n.name, n.shard))
		}
	}
	n.up = false
}

func (f *fleet) start(n *node, version string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n.up, n.version = true, version
}

// health is what the health reporter can see of one node.
type health struct {
	name         string
	serving      bool
	peerCanServe bool
}

func (f *fleet) health() []health {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []health
	for _, n := range f.nodes {
		h := health{name: n.name, serving: n.up}
		for _, other := range f.nodes {
			if other != n && other.shard == n.shard && other.up {
				h.peerCanServe = true
			}
		}
		out = append(out, h)
	}
	return out
}

func (f *fleet) summary() (versions string, violations []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var parts []string
	for _, n := range f.nodes {
		parts = append(parts, n.name+"="+n.version)
	}
	return strings.Join(parts, " "), append([]string(nil), f.violations...)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "example failed:", err)
		os.Exit(1)
	}
}

func run() error {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		return errors.New("KUBEBUILDER_ASSETS is not set; run `make example`")
	}
	ctrl.SetLogger(logr.Discard())
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// A local API server with the Allostat resource types.
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("config", "crd")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		return fmt.Errorf("start the API server: %w", err)
	}
	defer func() { _ = env.Stop() }()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return err
	}
	k8s, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}

	// The coordinator. It never sees the fleet, only the API server.
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}})
	if err != nil {
		return err
	}
	coordinator := &controller.ActionRequestReconciler{
		Reader: mgr.GetAPIReader(), Writer: mgr.GetClient(), Now: time.Now,
		ReleaseRetry: time.Second,
	}
	if err := coordinator.SetupWithManager(mgr); err != nil {
		return err
	}
	coordinatorDone := make(chan error, 1)
	go func() { coordinatorDone <- mgr.Start(ctx) }()

	// The rules of the fleet.
	if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
		return err
	}
	contract := &v1alpha1.Contract{
		ObjectMeta: metav1.ObjectMeta{Name: "restart", Namespace: namespace},
		Spec: v1alpha1.ContractSpec{
			Action: "restart",
			// The other replica of the shard must be able to serve.
			Requires: []v1alpha1.Requirement{{Condition: "PeerCanServe", MaxAge: metav1.Duration{Duration: 3 * time.Second}}},
			// One node of a shard may be out at a time.
			Budgets:  []v1alpha1.Budget{{Label: "shard", Max: 1}},
			GrantTTL: metav1.Duration{Duration: grantTTL},
			// A grant with a late or failed report is closed when the node
			// is seen serving again.
			ReleaseWhen: []v1alpha1.Requirement{{Condition: "TargetServing", MaxAge: metav1.Duration{Duration: 3 * time.Second}}},
		},
	}
	if err := k8s.Create(ctx, contract); err != nil {
		return err
	}
	started = time.Now()
	say("contract", "restart needs a fresh PeerCanServe; one node of a shard at a time; report within %s", grantTTL)

	f := newFleet()
	go reportHealth(ctx, k8s, f)
	// Let the first observations arrive.
	time.Sleep(500 * time.Millisecond)

	var workers sync.WaitGroup
	errs := make(chan error, len(f.nodes))
	for _, n := range f.nodes {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if err := upgrade(ctx, k8s, f, n); err != nil {
				errs <- fmt.Errorf("%s: %w", n.name, err)
			}
		}()
		// The "a" nodes ask first, so the lost report blocks a shard.
		time.Sleep(100 * time.Millisecond)
	}
	workers.Wait()
	close(errs)
	for err := range errs {
		return err
	}

	versions, violations := f.summary()
	say("result", "%s", versions)
	if len(violations) > 0 {
		return fmt.Errorf("a shard was left without a serving node: %s", strings.Join(violations, "; "))
	}
	if strings.Contains(versions, "=v1") {
		return errors.New("not every node was upgraded")
	}
	var lost v1alpha1.ActionRequest
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "restart-" + lostReport + "-1"}, &lost); err != nil {
		return err
	}
	if lost.Status.Released == nil {
		return fmt.Errorf("the grant of %s was not closed by evidence", lostReport)
	}
	say("result", "no shard was ever without a serving node")

	cancel()
	if err := <-coordinatorDone; err != nil {
		return fmt.Errorf("coordinator: %w", err)
	}
	return nil
}

// reportHealth is the health reporter. Twice a second it writes what it
// sees of every node. It knows nothing about upgrades.
func reportHealth(ctx context.Context, k8s client.Client, f *fleet) {
	status := func(ok bool) metav1.ConditionStatus {
		if ok {
			return metav1.ConditionTrue
		}
		return metav1.ConditionFalse
	}
	for ctx.Err() == nil {
		now := metav1.NewMicroTime(time.Now().UTC().Truncate(time.Microsecond))
		for _, h := range f.health() {
			facts := map[string]bool{"PeerCanServe": h.peerCanServe, "TargetServing": h.serving}
			for condition, ok := range facts {
				spec := v1alpha1.ObservationSpec{
					Condition: condition, Subject: h.name, Status: status(ok),
					Source: "health-reporter", ObservedAt: now,
				}
				if err := writeObservation(ctx, k8s, strings.ToLower(condition)+"-"+h.name, spec); err != nil && ctx.Err() == nil {
					say("health", "cannot report %s for %s: %v", condition, h.name, err)
				}
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// writeObservation keeps one object for each fact and writes the newest
// value into it.
func writeObservation(ctx context.Context, k8s client.Client, name string, spec v1alpha1.ObservationSpec) error {
	obs := &v1alpha1.Observation{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	err := k8s.Get(ctx, client.ObjectKeyFromObject(obs), obs)
	if apierrors.IsNotFound(err) {
		obs.Spec = spec
		return k8s.Create(ctx, obs)
	}
	if err != nil {
		return err
	}
	obs.Spec = spec
	return k8s.Update(ctx, obs)
}

// upgrade is the worker of the upgrade controller for one node. It asks,
// and acts only on a grant. A refusal is final, so the next try is a new
// request.
func upgrade(ctx context.Context, k8s client.Client, f *fleet, n *node) error {
	who := "upgrade " + n.name
	for attempt := 1; ; attempt++ {
		// The worker waits this long for an answer. A later answer is a
		// refusal, so the request can be left behind.
		waitUntil := metav1.NewTime(time.Now().Add(answerWait))
		req := &v1alpha1.ActionRequest{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("restart-%s-%d", n.name, attempt), Namespace: namespace},
			Spec: v1alpha1.ActionRequestSpec{
				Action: "restart", Requester: "upgrade-controller",
				Target:    v1alpha1.Target{Name: n.name, Labels: map[string]string{"shard": n.shard}},
				ExpiresAt: &waitUntil,
			},
		}
		if err := k8s.Create(ctx, req); err != nil {
			return err
		}
		answer, stop := context.WithDeadline(ctx, waitUntil.Time)
		err := waitFor(answer, k8s, req, func() bool { return req.Status.Decision != "" })
		stop()
		if err != nil {
			// No answer means no action.
			return fmt.Errorf("no answer from the coordinator: %w", err)
		}
		if req.Status.Decision != v1alpha1.DecisionGranted {
			say(who, "refused: %s", strings.Join(req.Status.Reasons, "; "))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
			continue
		}

		say(who, "granted, restarting")
		f.stop(n)
		time.Sleep(restartTime)
		f.start(n, newVersion)

		if n.name == lostReport {
			say(who, "is back on %s, but the worker stops before it reports", newVersion)
			// Somebody must notice. Here it is the coordinator, from what
			// the health reporter sees after the deadline.
			if err := waitFor(ctx, k8s, req, func() bool { return req.Status.Released != nil }); err != nil {
				return fmt.Errorf("the grant was not closed by evidence: %w", err)
			}
			say("coordinator", "closed the late grant of %s: %s", n.name, strings.Join(req.Status.Released.Evidence, "; "))
			return nil
		}

		req.Spec.Result = &v1alpha1.Result{Outcome: v1alpha1.OutcomeSucceeded}
		if err := k8s.Update(ctx, req); err != nil {
			return fmt.Errorf("report the result: %w", err)
		}
		say(who, "is back on %s, reported success", newVersion)
		return nil
	}
}

// waitFor reads the request again until done says yes.
func waitFor(ctx context.Context, k8s client.Client, req *v1alpha1.ActionRequest, done func() bool) error {
	for {
		if err := k8s.Get(ctx, client.ObjectKeyFromObject(req), req); err != nil {
			return err
		}
		if done() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
