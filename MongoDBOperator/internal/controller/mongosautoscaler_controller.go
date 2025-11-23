/*
Copyright 2025 Chen-Si-An.

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
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	autoscalerv1alpha1 "github.com/Chen-Si-An/Autoscaling/MongoDBOperator/api/v1alpha1"
	"github.com/Chen-Si-An/Autoscaling/MongoDBOperator/pkg/promclient"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MongosAutoscalerReconciler reconciles a MongosAutoscaler object
type MongosAutoscalerReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=autoscaler.mongodb.io,resources=mongosautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaler.mongodb.io,resources=mongosautoscalers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=autoscaler.mongodb.io,resources=mongosautoscalers/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the MongosAutoscaler object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.1/pkg/reconcile
func (r *MongosAutoscalerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	msa := &autoscalerv1alpha1.MongosAutoscaler{}
	if err := r.Get(ctx, req.NamespacedName, msa); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var deploy appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{
		Name:      msa.Spec.Router.Name,
		Namespace: msa.Spec.Router.Namespace,
	}, &deploy); err != nil {
		log.Error(err, "failed to get mongos deployment")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if deploy.Spec.Replicas == nil {
		log.Error(nil, "mongos deployment has nil replicas")
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	prom, err := promclient.NewPromClient(msa.Spec.Prometheus.URL)
	if err != nil {
		log.Error(err, "failed to init Prometheus client")
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	avgCPU, err := prom.QueryAvgCPU(ctx, msa.Spec.Router.Namespace, msa.Spec.Router.Name, msa.Spec.Policy.Window)
	if err != nil {
		log.Error(err, "prometheus query failed")
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	min := msa.Spec.ScaleBounds.MinReplicas
	max := msa.Spec.ScaleBounds.MaxReplicas
	curr := *deploy.Spec.Replicas
	target := float64(msa.Spec.Policy.CpuTargetPercent)
	tol := float64(msa.Spec.Policy.TolerancePercent)
	cooldown := time.Duration(msa.Spec.Policy.CooldownSeconds) * time.Second

	if time.Since(msa.Status.LastScaleTime.Time) < cooldown {
		return ctrl.Result{RequeueAfter: 45 * time.Second}, nil
	}

	desired := curr
	switch {
	case avgCPU > target+tol && curr < max:
		desired = curr + 1
		log.Info("Scaling up mongos", "from", curr, "to", desired, "cpu", avgCPU)
	case avgCPU < target-tol && curr > min:
		desired = curr - 1
		log.Info("Scaling down mongos", "from", curr, "to", desired, "cpu", avgCPU)
	default:
		log.Info("Mongos replicas within target range, no scaling action needed", "currentReplicas", curr, "cpu", avgCPU)
	}

	if desired != curr {
		deploy.Spec.Replicas = &desired
		if err := r.Update(ctx, &deploy); err != nil {
			log.Error(err, "failed to update mongos deployment replicas")
			return ctrl.Result{RequeueAfter: time.Minute}, nil
		}

		msa.Status.LastScaleTime = metav1.Now()
		msa.Status.LastObservedCPU = fmt.Sprintf("%f", avgCPU)
		msa.Status.LastDesiredReplicas = desired
		_ = r.Status().Update(ctx, msa)
		return ctrl.Result{}, nil
	}

	return ctrl.Result{RequeueAfter: 45 * time.Second}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *MongosAutoscalerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&autoscalerv1alpha1.MongosAutoscaler{}).
		Named("mongosautoscaler").
		Complete(r)
}
