// Package controller answers action requests.
package controller

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/JPaiv/Allostat/admission"
	"github.com/JPaiv/Allostat/api/v1alpha1"
)

// retryAfter is the wait before a request is decided again after another
// writer changed the ledger first.
const retryAfter = 100 * time.Millisecond

// releaseRetryAfter is the wait before the coordinator looks again for
// evidence that closes an overdue or failed grant.
const releaseRetryAfter = 30 * time.Second

// ActionRequestReconciler decides each new request once and writes the
// answer to its status. It never acts on a target.
//
// It keeps no state of its own. Everything a decision uses is read from the
// API server at the time of the decision, not from a cache.
//
// A grant is written in two steps:
//
//  1. It is added to the ledger of the namespace. The API server accepts
//     this write only if nobody changed the ledger since it was read.
//  2. It is written to the status of the request.
//
// After a grant the coordinator follows the request. If the grant becomes
// overdue or is reported as failed, and the contract has release
// conditions, fresh evidence can close it. See admission.Release.
//
// Step 1 is the guard. Two coordinators that decide from the same state
// cannot both pass it: the second write fails and that request is decided
// again, now with the first grant in the ledger. If the coordinator stops
// between the steps, the next run finds the grant in the ledger and
// finishes step 2.
type ActionRequestReconciler struct {
	// Reader must read from the API server directly.
	Reader client.Reader
	// Writer creates the ledger and writes the ledger and request status.
	Writer client.Client
	// Now is the clock. It can be replaced in tests.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=allostat.io,resources=contracts;observations;actionrequests,verbs=get;list;watch
// +kubebuilder:rbac:groups=allostat.io,resources=actionrequests/status,verbs=update
// +kubebuilder:rbac:groups=allostat.io,resources=ledgers,verbs=get;create
// +kubebuilder:rbac:groups=allostat.io,resources=ledgers/status,verbs=update
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;create;update
// +kubebuilder:rbac:groups="";events.k8s.io,resources=events,verbs=create;patch

// Reconcile decides one request if it has no decision yet, and follows a
// grant until it is closed.
//
// A decision is final. A refused request stays refused; the requester asks
// again with a new request.
func (r *ActionRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ar v1alpha1.ActionRequest
	if err := r.Reader.Get(ctx, req.NamespacedName, &ar); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !ar.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	if ar.Status.Decision != "" {
		return r.followGrant(ctx, &ar)
	}

	// The ledger is read first. Everything read after it is at least as
	// new, and the ledger write at the end fails if a grant was added in
	// between.
	ledger, err := r.ledger(ctx, ar.Namespace)
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			return ctrl.Result{RequeueAfter: retryAfter}, nil
		}
		return ctrl.Result{}, err
	}
	var requests v1alpha1.ActionRequestList
	if err := r.Reader.List(ctx, &requests, client.InNamespace(ar.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	kept, open := openGrants(ledger, requests.Items)

	// The ledger already has a grant for this request: an earlier run
	// stopped before it wrote the status. Finish it.
	for _, g := range kept {
		if g.UID == ar.UID {
			return ctrl.Result{}, r.writeDecision(ctx, &ar, nil, g.GrantedAt, &g.Deadline)
		}
	}

	// The API server stores times with microseconds.
	now := r.Now().UTC().Truncate(time.Microsecond)
	decision, err := r.decide(ctx, &ar, open, now)
	if err != nil {
		return ctrl.Result{}, err
	}
	decidedAt := metav1.NewMicroTime(now)
	if !decision.Granted {
		return ctrl.Result{}, r.writeDecision(ctx, &ar, decision.Reasons, decidedAt, nil)
	}

	deadline := metav1.NewMicroTime(decision.Grant.Deadline)
	ledger.Status.Grants = append(kept, v1alpha1.LedgerGrant{
		Request: ar.Name, UID: ar.UID, GrantedAt: decidedAt, Deadline: deadline,
	})
	if err := r.Writer.Status().Update(ctx, ledger); err != nil {
		if apierrors.IsConflict(err) {
			// Another writer changed the ledger. Decide again from fresh data.
			log.FromContext(ctx).Info("ledger changed during the decision, deciding again")
			return ctrl.Result{RequeueAfter: retryAfter}, nil
		}
		return ctrl.Result{}, err
	}
	if err := r.writeDecision(ctx, &ar, nil, decidedAt, &deadline); err != nil {
		return ctrl.Result{}, err
	}
	// Look again when the grant is overdue.
	return ctrl.Result{RequeueAfter: decision.Grant.Deadline.Sub(now) + time.Microsecond}, nil
}

// followGrant looks at a request that has a decision. It closes an overdue
// or failed grant if the contract allows that and the evidence is there.
// Until then it asks to be called again.
func (r *ActionRequestReconciler) followGrant(ctx context.Context, ar *v1alpha1.ActionRequest) (ctrl.Result, error) {
	grant, open := ar.OpenGrant()
	if !open {
		return ctrl.Result{}, nil
	}
	now := r.Now().UTC().Truncate(time.Microsecond)

	if grant.Failed && grant.FailedAt.IsZero() {
		// Only evidence that is newer than this time can close the grant.
		seenAt := metav1.NewMicroTime(now)
		ar.Status.FailureSeenAt = &seenAt
		if err := r.Writer.Status().Update(ctx, ar); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: releaseRetryAfter}, nil
	}
	if !grant.Failed && !grant.Overdue(now) {
		// Nothing to do before the deadline.
		return ctrl.Result{RequeueAfter: grant.Deadline.Sub(now) + time.Microsecond}, nil
	}

	contract, reason, err := r.contract(ctx, ar)
	if err != nil {
		return ctrl.Result{}, err
	}
	if reason != "" {
		// No contract, no release.
		return ctrl.Result{RequeueAfter: releaseRetryAfter}, nil
	}
	obs, err := r.observations(ctx, ar.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	release := admission.Release(contract.ToAdmission(), grant, obs, now)
	if !release.Released {
		return ctrl.Result{RequeueAfter: releaseRetryAfter}, nil
	}

	ar.Status.Released = &v1alpha1.Release{At: metav1.NewMicroTime(now)}
	for _, o := range release.Evidence {
		ar.Status.Released.Evidence = append(ar.Status.Released.Evidence, fmt.Sprintf(
			"%s=%s for %q from %q, measured at %s",
			o.Condition, o.Status, o.Subject, o.Source, o.ObservedAt.UTC().Format(time.RFC3339Nano)))
	}
	// The update fails if the request changed after it was read, for
	// example by a late report. The request is then looked at again.
	if err := r.Writer.Status().Update(ctx, ar); err != nil {
		return ctrl.Result{}, err
	}
	log.FromContext(ctx).Info("released by evidence",
		"action", ar.Spec.Action,
		"target", ar.Spec.Target.Name,
		"requester", ar.Spec.Requester,
		"failed", grant.Failed,
		"evidence", ar.Status.Released.Evidence,
	)
	return ctrl.Result{}, nil
}

// ledger returns the ledger of a namespace and creates it if it is missing.
func (r *ActionRequestReconciler) ledger(ctx context.Context, namespace string) (*v1alpha1.Ledger, error) {
	ledger := &v1alpha1.Ledger{}
	err := r.Reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: v1alpha1.LedgerName}, ledger)
	if apierrors.IsNotFound(err) {
		ledger = &v1alpha1.Ledger{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: v1alpha1.LedgerName}}
		err = r.Writer.Create(ctx, ledger)
	}
	return ledger, err
}

// openGrants returns the ledger entries to keep and the open grants.
//
// A ledger entry is dropped only when its request is gone, reported
// success, or was released by evidence. A grant that is in a request status but not in the ledger is
// counted too and put back, so a lost ledger cannot free a budget.
func openGrants(ledger *v1alpha1.Ledger, requests []v1alpha1.ActionRequest) ([]v1alpha1.LedgerGrant, []admission.Grant) {
	byName := make(map[string]*v1alpha1.ActionRequest, len(requests))
	for i := range requests {
		byName[requests[i].Name] = &requests[i]
	}

	var kept []v1alpha1.LedgerGrant
	var open []admission.Grant
	inLedger := make(map[string]bool, len(ledger.Status.Grants))
	for _, g := range ledger.Status.Grants {
		req, ok := byName[g.Request]
		if !ok || req.UID != g.UID || req.Closed() {
			continue
		}
		inLedger[g.Request] = true
		kept = append(kept, g)
		open = append(open, req.GrantAt(g.GrantedAt.Time, g.Deadline.Time))
	}
	for i := range requests {
		req := &requests[i]
		if inLedger[req.Name] {
			continue
		}
		if g, ok := req.OpenGrant(); ok {
			kept = append(kept, v1alpha1.LedgerGrant{
				Request: req.Name, UID: req.UID,
				GrantedAt: *req.Status.DecidedAt, Deadline: *req.Status.Deadline,
			})
			open = append(open, g)
		}
	}
	return kept, open
}

// decide reads the contract and the observations of the request's namespace
// and runs the decision core.
func (r *ActionRequestReconciler) decide(ctx context.Context, ar *v1alpha1.ActionRequest, open []admission.Grant, now time.Time) (admission.Decision, error) {
	contract, reason, err := r.contract(ctx, ar)
	if err != nil {
		return admission.Decision{}, err
	}
	if reason != "" {
		return admission.Decision{Reasons: []string{reason}}, nil
	}
	obs, err := r.observations(ctx, ar.Namespace)
	if err != nil {
		return admission.Decision{}, err
	}
	return admission.Decide(contract.ToAdmission(), ar.ToAdmission(), obs, open, now), nil
}

// contract returns the one contract for the request's action in its
// namespace. If there is not exactly one, it returns the reason.
func (r *ActionRequestReconciler) contract(ctx context.Context, ar *v1alpha1.ActionRequest) (*v1alpha1.Contract, string, error) {
	var contracts v1alpha1.ContractList
	if err := r.Reader.List(ctx, &contracts, client.InNamespace(ar.Namespace)); err != nil {
		return nil, "", err
	}
	var matching []*v1alpha1.Contract
	for i := range contracts.Items {
		if contracts.Items[i].Spec.Action == ar.Spec.Action {
			matching = append(matching, &contracts.Items[i])
		}
	}
	switch len(matching) {
	case 0:
		return nil, fmt.Sprintf("no contract covers action %q in namespace %q", ar.Spec.Action, ar.Namespace), nil
	case 1:
		return matching[0], "", nil
	default:
		// With two contracts it is not clear which rules apply.
		return nil, fmt.Sprintf("%d contracts cover action %q in namespace %q, want exactly one", len(matching), ar.Spec.Action, ar.Namespace), nil
	}
}

func (r *ActionRequestReconciler) observations(ctx context.Context, namespace string) ([]admission.Observation, error) {
	var observations v1alpha1.ObservationList
	if err := r.Reader.List(ctx, &observations, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	obs := make([]admission.Observation, 0, len(observations.Items))
	for i := range observations.Items {
		obs = append(obs, observations.Items[i].ToAdmission())
	}
	return obs, nil
}

// writeDecision writes the answer to the request status. A deadline makes
// it a grant.
func (r *ActionRequestReconciler) writeDecision(ctx context.Context, ar *v1alpha1.ActionRequest, reasons []string, decidedAt metav1.MicroTime, deadline *metav1.MicroTime) error {
	ar.Status = v1alpha1.ActionRequestStatus{
		Decision:  v1alpha1.DecisionRefused,
		Reasons:   reasons,
		DecidedAt: &decidedAt,
	}
	if deadline != nil {
		ar.Status.Decision = v1alpha1.DecisionGranted
		ar.Status.Deadline = deadline
	}
	// The update fails if the request changed after it was read. The
	// request is then looked at again.
	if err := r.Writer.Status().Update(ctx, ar); err != nil {
		return err
	}
	log.FromContext(ctx).Info("decided",
		"decision", ar.Status.Decision,
		"action", ar.Spec.Action,
		"target", ar.Spec.Target.Name,
		"requester", ar.Spec.Requester,
		"reasons", ar.Status.Reasons,
	)
	return nil
}

// SetupWithManager registers the reconciler. Requests are decided one at a
// time.
func (r *ActionRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.ActionRequest{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}
