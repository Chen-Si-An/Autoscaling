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

	cooldown := time.Duration(msa.Spec.Policy.CooldownSeconds) * time.Second
	if time.Since(msa.Status.LastScaleTime.Time) < cooldown {
		return ctrl.Result{RequeueAfter: 45 * time.Second}, nil
	}

	cpuTarget := float64(msa.Spec.Policy.CpuTargetPercent)
	cpuTol := float64(msa.Spec.Policy.CpuTolerancePercent)

	prom, err := promclient.NewPromClient(msa.Spec.Prometheus.URL)
	if err != nil {
		log.Error(err, "failed to init Prometheus client")
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	podRegex := fmt.Sprintf("%s.*", msa.Spec.Router.Name)
	avgCPU, err := prom.QueryAvgCPU(ctx, msa.Spec.Router.Namespace, podRegex, msa.Spec.Policy.Window)
	if err != nil {
		log.Error(err, "prometheus query failed")
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	minReplicas := msa.Spec.ScaleBounds.MinReplicas
	maxReplicas := msa.Spec.ScaleBounds.MaxReplicas
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
	curReplicas := *deploy.Spec.Replicas

	desiredReplicas := curReplicas
	switch {
	case avgCPU > cpuTarget+cpuTol && curReplicas < maxReplicas:
		desiredReplicas = curReplicas + 1
		log.Info("Scaling up mongos", "cpu", avgCPU, "oldReplicas", curReplicas, "newReplicas", desiredReplicas)
	case avgCPU < cpuTarget-cpuTol && curReplicas > minReplicas:
		desiredReplicas = curReplicas - 1
		log.Info("Scaling down mongos", "cpu", avgCPU, "oldReplicas", curReplicas, "newReplicas", desiredReplicas)
	default:
		log.Info("No router scaling action", "cpu", avgCPU, "replicas", curReplicas)
		return ctrl.Result{RequeueAfter: 45 * time.Second}, nil
	}

	deploy.Spec.Replicas = &desiredReplicas
	if err := r.Update(ctx, &deploy); err != nil {
		log.Error(err, "failed to update mongos deployment replicas")
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	msa.Status.LastObservedCPU = fmt.Sprintf("%f", avgCPU)
	msa.Status.LastDesiredReplicas = desiredReplicas
	msa.Status.LastScaleTime = metav1.Now()
	_ = r.Status().Update(ctx, msa)
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *MongosAutoscalerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&autoscalerv1alpha1.MongosAutoscaler{}).
		Named("mongosautoscaler").
		Complete(r)
}
