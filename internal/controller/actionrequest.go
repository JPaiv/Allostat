// Package controller answers action requests.
package controller

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/JPaiv/Allostat/admission"
	"github.com/JPaiv/Allostat/api/v1alpha1"
)

// ActionRequestReconciler decides each new request once and writes the
// answer to its status. It never acts on a target.
//
// It keeps no state of its own. Everything a decision uses is read from the
// API server at the time of the decision.
//
// Two rules keep the budgets correct:
//
//   - Requests are decided one at a time.
//   - Reads go to the API server, not to a cache. A grant that was written a
//     moment ago is always seen by the next decision.
//
// Both rules hold only for one running coordinator. Run it with leader
// election.
type ActionRequestReconciler struct {
	// Reader must read from the API server directly.
	Reader client.Reader
	// Status writes the decision.
	Status client.SubResourceWriter
	// Now is the clock. It can be replaced in tests.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=allostat.io,resources=contracts;observations;actionrequests,verbs=get;list;watch
// +kubebuilder:rbac:groups=allostat.io,resources=actionrequests/status,verbs=update
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;create;update
// +kubebuilder:rbac:groups="";events.k8s.io,resources=events,verbs=create;patch

// Reconcile decides one request if it has no decision yet.
//
// A decision is final. A refused request stays refused; the requester asks
// again with a new request.
func (r *ActionRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ar v1alpha1.ActionRequest
	if err := r.Reader.Get(ctx, req.NamespacedName, &ar); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if ar.Status.Decision != "" || !ar.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// The API server stores times with microseconds.
	now := r.Now().UTC().Truncate(time.Microsecond)
	decision, err := r.decide(ctx, &ar, now)
	if err != nil {
		return ctrl.Result{}, err
	}

	decidedAt := metav1.NewMicroTime(now)
	ar.Status = v1alpha1.ActionRequestStatus{
		Decision:  v1alpha1.DecisionRefused,
		Reasons:   decision.Reasons,
		DecidedAt: &decidedAt,
	}
	if decision.Granted {
		deadline := metav1.NewMicroTime(decision.Grant.Deadline)
		ar.Status.Decision = v1alpha1.DecisionGranted
		ar.Status.Deadline = &deadline
	}
	// The update fails if the request changed after it was read. The
	// request is then decided again from fresh data.
	if err := r.Status.Update(ctx, &ar); err != nil {
		return ctrl.Result{}, err
	}

	log.FromContext(ctx).Info("decided",
		"decision", ar.Status.Decision,
		"action", ar.Spec.Action,
		"target", ar.Spec.Target.Name,
		"requester", ar.Spec.Requester,
		"reasons", ar.Status.Reasons,
	)
	return ctrl.Result{}, nil
}

// decide reads the contract, the observations and the open grants of the
// request's namespace and runs the decision core.
func (r *ActionRequestReconciler) decide(ctx context.Context, ar *v1alpha1.ActionRequest, now time.Time) (admission.Decision, error) {
	inNamespace := client.InNamespace(ar.Namespace)

	var contracts v1alpha1.ContractList
	if err := r.Reader.List(ctx, &contracts, inNamespace); err != nil {
		return admission.Decision{}, err
	}
	var matching []*v1alpha1.Contract
	for i := range contracts.Items {
		if contracts.Items[i].Spec.Action == ar.Spec.Action {
			matching = append(matching, &contracts.Items[i])
		}
	}
	switch len(matching) {
	case 0:
		return refuse("no contract covers action %q in namespace %q", ar.Spec.Action, ar.Namespace), nil
	case 1:
	default:
		// With two contracts it is not clear which rules apply.
		return refuse("%d contracts cover action %q in namespace %q, want exactly one", len(matching), ar.Spec.Action, ar.Namespace), nil
	}

	var observations v1alpha1.ObservationList
	if err := r.Reader.List(ctx, &observations, inNamespace); err != nil {
		return admission.Decision{}, err
	}
	obs := make([]admission.Observation, 0, len(observations.Items))
	for i := range observations.Items {
		obs = append(obs, observations.Items[i].ToAdmission())
	}

	var requests v1alpha1.ActionRequestList
	if err := r.Reader.List(ctx, &requests, inNamespace); err != nil {
		return admission.Decision{}, err
	}
	var open []admission.Grant
	for i := range requests.Items {
		if g, ok := requests.Items[i].OpenGrant(); ok {
			open = append(open, g)
		}
	}

	return admission.Decide(matching[0].ToAdmission(), ar.ToAdmission(), obs, open, now), nil
}

func refuse(format string, args ...any) admission.Decision {
	return admission.Decision{Reasons: []string{fmt.Sprintf(format, args...)}}
}

// SetupWithManager registers the reconciler. Requests are decided one at a
// time.
func (r *ActionRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.ActionRequest{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}
