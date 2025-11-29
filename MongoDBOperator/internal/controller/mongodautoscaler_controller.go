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
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	autoscalerv1alpha1 "github.com/Chen-Si-An/Autoscaling/MongoDBOperator/api/v1alpha1"
	"github.com/Chen-Si-An/Autoscaling/MongoDBOperator/pkg/promclient"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MongodAutoscalerReconciler reconciles a MongodAutoscaler object
type MongodAutoscalerReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=autoscaler.mongodb.io,resources=mongodautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaler.mongodb.io,resources=mongodautoscalers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=autoscaler.mongodb.io,resources=mongodautoscalers/finalizers,verbs=update
// +kubebuilder:rbac:groups="", resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the MongodAutoscaler object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.1/pkg/reconcile
func (r *MongodAutoscalerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	mda := &autoscalerv1alpha1.MongodAutoscaler{}
	if err := r.Get(ctx, req.NamespacedName, mda); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if mda.Status.ScalingPhase == "" {
		mda.Status.ScalingPhase = "Idle"
	}

	cooldown := time.Duration(mda.Spec.Policy.CooldownSeconds) * time.Second
	if time.Since(mda.Status.LastScaleTime.Time) < cooldown {
		return ctrl.Result{RequeueAfter: 45 * time.Second}, nil
	}

	cpuTarget := float64(mda.Spec.Policy.CpuTargetPercent)
	cpuTol := float64(mda.Spec.Policy.CpuTolerancePercent)
	iowaitTarget := float64(mda.Spec.Policy.IOWaitTargetPercent)
	iowaitTol := float64(mda.Spec.Policy.IOWaitTolerancePercent)

	if mda.Status.ScalingPhase == "Idle" {
		balancerRunning, err := r.isBalancerRunning(ctx, mda)
		if err != nil {
			log.Error(err, "failed to check balancer status")
			return ctrl.Result{RequeueAfter: time.Minute}, nil
		}
		if balancerRunning {
			log.Info("Balancer is currently running, skipping scaling checks")
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		prom, err := promclient.NewPromClient(mda.Spec.Prometheus.URL)
		if err != nil {
			log.Error(err, "failed to init Prometheus client")
			return ctrl.Result{RequeueAfter: time.Minute}, nil
		}

		podRegex := fmt.Sprintf("%s-shard[0-9]+-data.*", mda.Spec.Bitnami.ReleaseName)
		avgCPU, err := prom.QueryAvgCPU(ctx, mda.Spec.Bitnami.Namespace, podRegex, mda.Spec.Policy.Window)
		if err != nil {
			log.Error(err, "prometheus query failed")
			return ctrl.Result{RequeueAfter: time.Minute}, nil
		}

		ips, err := r.getDatanodeNodeIPs(ctx, mda)
		if err != nil {
			log.Error(err, "failed to get datanode node IPs")
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		endpoints := make([]string, 0, len(ips))
		for _, ip := range ips {
			endpoint := fmt.Sprintf("%s:%d", ip, mda.Spec.Prometheus.NodeExporterPort)
			endpoints = append(endpoints, endpoint)
		}

		avgIOWait, err := prom.QueryAvgIOWait(ctx, endpoints, mda.Spec.Policy.Window)
		if err != nil {
			log.Error(err, "prometheus query failed")
			return ctrl.Result{RequeueAfter: time.Minute}, nil
		}

		var minShards, maxShards, minReplicas, maxReplicas int32
		if mda.Spec.ShardScaleBounds != nil {
			minShards = mda.Spec.ShardScaleBounds.MinShards
			maxShards = mda.Spec.ShardScaleBounds.MaxShards
		}
		if mda.Spec.ReplicaScaleBounds != nil {
			minReplicas = mda.Spec.ReplicaScaleBounds.MinReplicas
			maxReplicas = mda.Spec.ReplicaScaleBounds.MaxReplicas
		}
		curShards, err := r.currentShardCount(ctx, mda)
		if err != nil {
			log.Error(err, "failed to get current shard count")
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		curReplicas, err := r.currentReplicaCount(ctx, mda)
		if err != nil {
			log.Error(err, "failed to get current replica count")
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		switch {
		case avgCPU > cpuTarget+cpuTol || avgIOWait > iowaitTarget+iowaitTol:
			if mda.Spec.ShardScaleBounds != nil && curShards < maxShards {
				mda.Status.LastObservedCPU = fmt.Sprintf("%f", avgCPU)
				mda.Status.LastDesiredShards = curShards + 1
				mda.Status.LastDesiredReplicas = curReplicas
				mda.Status.ScalingPhase = "ScalingUpCreatingShard"
				mda.Status.TargetShardIdx = curShards
				log.Info("Scaling UP shards", "cpu", avgCPU, "iowait", avgIOWait, "oldShards", curShards, "newShards", curShards+1)
				_ = r.Status().Update(ctx, mda)
				return ctrl.Result{}, nil
			} else if mda.Spec.ReplicaScaleBounds != nil && curReplicas < maxReplicas {
				mda.Status.LastObservedCPU = fmt.Sprintf("%f", avgCPU)
				mda.Status.LastDesiredShards = curShards
				mda.Status.LastDesiredReplicas = curReplicas + 1
				mda.Status.ScalingPhase = "ScalingUpCreatingReplica"
				mda.Status.TargetReplicaCount = curReplicas + 1
				log.Info("Scaling UP replicas", "cpu", avgCPU, "iowait", avgIOWait, "oldReplicas", curReplicas, "newReplicas", curReplicas+1)
				_ = r.Status().Update(ctx, mda)
				return ctrl.Result{}, nil
			} else {
				log.Info("No shard scaling action", "cpu", avgCPU, "iowait", avgIOWait, "shards", curShards, "replicas", curReplicas)
				return ctrl.Result{RequeueAfter: 45 * time.Second}, nil
			}
		case avgCPU < cpuTarget-cpuTol && avgIOWait < iowaitTarget-iowaitTol:
			if mda.Spec.ShardScaleBounds != nil && curReplicas > minReplicas {
				mda.Status.LastObservedCPU = fmt.Sprintf("%f", avgCPU)
				mda.Status.LastDesiredShards = curShards
				mda.Status.LastDesiredReplicas = curReplicas - 1
				mda.Status.ScalingPhase = "ScalingDownRemovingReplica"
				mda.Status.TargetReplicaCount = curReplicas - 1
				log.Info("Scaling DOWN replicas", "cpu", avgCPU, "iowait", avgIOWait, "oldReplicas", curReplicas, "newReplicas", curReplicas-1)
				_ = r.Status().Update(ctx, mda)
				return ctrl.Result{}, nil
			} else if mda.Spec.ReplicaScaleBounds != nil && curShards > minShards {
				mda.Status.LastObservedCPU = fmt.Sprintf("%f", avgCPU)
				mda.Status.LastDesiredShards = curShards - 1
				mda.Status.LastDesiredReplicas = curReplicas
				mda.Status.ScalingPhase = "ScalingDownRemovingShard"
				mda.Status.TargetShardIdx = curShards - 1
				log.Info("Scaling DOWN shards", "cpu", avgCPU, "iowait", avgIOWait, "oldShards", curShards, "newShards", curShards-1)
				_ = r.Status().Update(ctx, mda)
				return ctrl.Result{}, nil
			} else {
				log.Info("No shard scaling action", "cpu", avgCPU, "iowait", avgIOWait, "shards", curShards, "replicas", curReplicas)
				return ctrl.Result{RequeueAfter: 45 * time.Second}, nil
			}
		default:
			log.Info("No shard scaling action", "cpu", avgCPU, "iowait", avgIOWait, "shards", curShards, "replicas", curReplicas)
			return ctrl.Result{RequeueAfter: 45 * time.Second}, nil
		}
	}

	switch mda.Status.ScalingPhase {
	case "ScalingUpCreatingShard":
		shardIdx := int(mda.Status.TargetShardIdx)
		if err := r.createBitnamiShard(ctx, mda, shardIdx); err != nil {
			log.Error(err, "failed to create bitnami shard resources")
			return ctrl.Result{RequeueAfter: time.Minute}, nil
		} else {
			mda.Status.ScalingPhase = "ScalingUpAddingShard"
		}
	case "ScalingUpAddingShard":
		release := mda.Spec.Bitnami.ReleaseName
		shardIdx := int(mda.Status.TargetShardIdx)
		replSet := bitnamiReplicaSetName(release, shardIdx)

		expectedSTS := fmt.Sprintf("%s-shard%d-data", release, shardIdx)
		var shardSTS appsv1.StatefulSet
		if err := r.Get(ctx, client.ObjectKey{Namespace: mda.Spec.Bitnami.Namespace, Name: expectedSTS}, &shardSTS); err != nil {
			log.Info("Waiting for shard StatefulSet before addShard", "statefulset", expectedSTS)
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		if shardSTS.Status.ReadyReplicas == 0 {
			log.Info("Shard StatefulSet not ready yet; postponing addShard", "statefulset", expectedSTS)
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		if err := r.createAddBitnamiShardJob(ctx, mda, shardIdx); err != nil {
			log.Error(err, "failed to create addShard Job (bitnami)")
			return ctrl.Result{RequeueAfter: time.Minute}, nil
		}

		addJobName := fmt.Sprintf("addshard-%s", replSet)
		var addJob batchv1.Job
		if err := r.Get(ctx, client.ObjectKey{Namespace: mda.Spec.Bitnami.Namespace, Name: addJobName}, &addJob); err != nil {
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		if addJob.Status.Failed > 0 {
			log.Error(fmt.Errorf("addShard Job failed"), "addShard Job encountered failures", "replSet", replSet, "job", addJobName)
			foreground := metav1.DeletePropagationForeground
			_ = r.Delete(ctx, &addJob, &client.DeleteOptions{
				PropagationPolicy: &foreground,
			})
			return ctrl.Result{RequeueAfter: time.Minute}, nil
		}
		if addJob.Status.Succeeded < 1 {
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		log.Info("addShard Job completed successfully", "replSet", replSet, "job", addJobName)
		foreground := metav1.DeletePropagationForeground
		_ = r.Delete(ctx, &addJob, &client.DeleteOptions{
			PropagationPolicy: &foreground,
		})
		mda.Status.ScalingPhase = "ScalingUpWaitingForResharding"
	case "ScalingUpWaitingForResharding":
		balancerRunning, err := r.isBalancerRunning(ctx, mda)
		if err != nil {
			log.Error(err, "failed to check balancer status")
			return ctrl.Result{RequeueAfter: time.Minute}, nil
		} else if balancerRunning {
			log.Info("Waiting for resharding to complete before finishing scale-up")
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		log.Info("Resharding completed; shard scaling-up process finished")
		mda.Status.ScalingPhase = "Idle"
		mda.Status.TargetShardIdx = -1
		mda.Status.LastScaleTime = metav1.Now()
	case "ScalingDownRemovingShard":
		shardIdx := int(mda.Status.TargetShardIdx)
		release := mda.Spec.Bitnami.ReleaseName
		replSet := bitnamiReplicaSetName(release, shardIdx)

		// Ensure the Job exists (idempotent)
		if err := r.createRemoveBitnamiShardJob(ctx, mda, shardIdx); err != nil {
			log.Error(err, "failed to create removeShard Job (bitnami)")
			return ctrl.Result{RequeueAfter: time.Minute}, nil
		}

		// Wait for Job completion (which indicates draining is completed).
		rmJobName := fmt.Sprintf("removeshard-%s", replSet)
		var rmJob batchv1.Job
		if err := r.Get(ctx, client.ObjectKey{Namespace: mda.Spec.Bitnami.Namespace, Name: rmJobName}, &rmJob); err != nil {
			log.Info("Waiting for removeShard Job to be created", "job", rmJobName)
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		if rmJob.Status.Failed > 0 {
			log.Error(fmt.Errorf("removeShard Job failed"), "removeShard Job encountered failures", "replSet", replSet, "job", rmJobName)
			foreground := metav1.DeletePropagationForeground
			_ = r.Delete(ctx, &rmJob, &client.DeleteOptions{
				PropagationPolicy: &foreground,
			})
			return ctrl.Result{RequeueAfter: time.Minute}, nil
		}
		if rmJob.Status.Succeeded < 1 {
			log.Info("Waiting for shard drain to complete before deleting resources", "replSet", replSet, "job", rmJobName)
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		log.Info("removeShard Job completed successfully", "replSet", replSet, "job", rmJobName)
		foreground := metav1.DeletePropagationForeground
		_ = r.Delete(ctx, &rmJob, &client.DeleteOptions{
			PropagationPolicy: &foreground,
		})
		mda.Status.ScalingPhase = "ScalingDownDropDB"
	case "ScalingDownDropDB":
		shardIdx := int(mda.Status.TargetShardIdx)

		// Run a dropDB job on the shard's primary to clean up local data before deletion
		dbList, err := r.listShardDatabases(ctx, mda, shardIdx)
		if err != nil {
			log.Error(err, "failed to list databases for shard", "shardIndex", shardIdx)
			return ctrl.Result{RequeueAfter: time.Minute}, nil
		}
		if err := r.createDropDBJob(ctx, mda, shardIdx, dbList); err != nil {
			log.Error(err, "failed to create dropDB Job for databases", "databases", dbList)
			return ctrl.Result{RequeueAfter: time.Minute}, nil
		}

		dropJobName := fmt.Sprintf("dropdb-shard%d", shardIdx)
		var dropJob batchv1.Job
		if err := r.Get(ctx, client.ObjectKey{Namespace: mda.Spec.Bitnami.Namespace, Name: dropJobName}, &dropJob); err != nil {
			log.Info("Waiting for dropDB Job to be created", "job", dropJobName)
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		if dropJob.Status.Failed > 0 {
			log.Error(fmt.Errorf("dropDB Job failed"), "dropDB Job encountered failures", "shardIndex", shardIdx, "job", dropJobName)
			foreground := metav1.DeletePropagationForeground
			_ = r.Delete(ctx, &dropJob, &client.DeleteOptions{
				PropagationPolicy: &foreground,
			})
			return ctrl.Result{RequeueAfter: time.Minute}, nil
		}
		if dropJob.Status.Succeeded < 1 {
			log.Info("Waiting for dropDB job to complete before deleting resources", "job", dropJobName)
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		log.Info("dropDB Job completed successfully", "job", dropJobName)
		foreground := metav1.DeletePropagationForeground
		_ = r.Delete(ctx, &dropJob, &client.DeleteOptions{
			PropagationPolicy: &foreground,
		})
		mda.Status.ScalingPhase = "ScalingDownDeletingShard"
	case "ScalingDownDeletingShard":
		shardIdx := int(mda.Status.TargetShardIdx)

		if err := r.deleteBitnamiShard(ctx, mda, shardIdx); err != nil {
			log.Error(err, "failed to delete bitnami shard resources")
			return ctrl.Result{RequeueAfter: time.Minute}, nil
		}

		log.Info("Shard scaling-down process finished")
		mda.Status.ScalingPhase = "Idle"
		mda.Status.TargetShardIdx = -1
		mda.Status.LastScaleTime = metav1.Now()
	case "ScalingUpCreatingReplica":
		replicas := mda.Status.TargetReplicaCount
		if err := r.patchBitnamiReplica(ctx, mda, replicas); err != nil {
			log.Error(err, "failed to create bitnami replica")
			return ctrl.Result{RequeueAfter: time.Minute}, nil
		} else {
			mda.Status.ScalingPhase = "ScalingUpAddingReplica"
		}
	case "ScalingUpAddingReplica":
		replicas := mda.Status.TargetReplicaCount
		action := "add"

		stsList, err := r.currentBitnamiShards(ctx, mda)
		if err != nil {
			log.Error(err, "failed to get current shards")
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		// Ensure all shard replicas are ready
		for _, sts := range stsList.Items {
			if sts.Status.ReadyReplicas < *sts.Spec.Replicas {
				log.Info("Waiting for shard StatefulSet to have all replicas ready", "statefulset", sts.Name)
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}
		}

		// Check if mongos can connect to primarys of all shards
		for _, sts := range stsList.Items {
			if err := r.checkShardConnectivity(ctx, mda, sts); err != nil {
				log.Error(err, "mongos cannot connect to shard primary", "statefulset", sts.Name)
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}
		}

		for _, sts := range stsList.Items {
			if err := r.createReplicaMembershipJob(ctx, mda, sts, replicas-1, action); err != nil {
				log.Error(err, "failed to create membership Job (bitnami)", "statefulset", sts.Name)
				return ctrl.Result{RequeueAfter: time.Minute}, nil
			}
		}

		var memJobs batchv1.JobList
		for _, sts := range stsList.Items {
			memJobName := fmt.Sprintf("%s-membership-%s-%d", action, sts.Name, replicas-1)
			var memJob batchv1.Job
			if err := r.Get(ctx, client.ObjectKey{Namespace: mda.Spec.Bitnami.Namespace, Name: memJobName}, &memJob); err != nil {
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}

			if memJob.Status.Failed > 0 {
				log.Error(fmt.Errorf("membership Job failed"), "membership Job encountered failures", "statefulset", sts.Name, "replicaIndex", replicas-1, "job", memJobName)
				foreground := metav1.DeletePropagationForeground
				_ = r.Delete(ctx, &memJob, &client.DeleteOptions{
					PropagationPolicy: &foreground,
				})
				return ctrl.Result{RequeueAfter: time.Minute}, nil
			}

			if memJob.Status.Succeeded < 1 {
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}

			memJobs.Items = append(memJobs.Items, memJob)
		}

		log.Info("membership Jobs completed successfully", "replicaIndex", replicas-1)
		for _, memJob := range memJobs.Items {
			foreground := metav1.DeletePropagationForeground
			_ = r.Delete(ctx, &memJob, &client.DeleteOptions{
				PropagationPolicy: &foreground,
			})
		}

		log.Info("Replica scaling-up process finished")
		mda.Status.ScalingPhase = "Idle"
		mda.Status.TargetReplicaCount = -1
		mda.Status.LastScaleTime = metav1.Now()
	case "ScalingDownRemovingReplica":
		replicas := mda.Status.TargetReplicaCount
		action := "remove"

		// Check if mongos can connect to primarys of all shards
		stsList, err := r.currentBitnamiShards(ctx, mda)
		if err != nil {
			log.Error(err, "failed to get current shards")
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		for _, sts := range stsList.Items {
			if err := r.checkShardConnectivity(ctx, mda, sts); err != nil {
				log.Error(err, "mongos cannot connect to shard primary", "statefulset", sts.Name)
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}
		}

		for _, sts := range stsList.Items {
			if err := r.createReplicaMembershipJob(ctx, mda, sts, replicas, action); err != nil {
				log.Error(err, "failed to create membership Job (bitnami)", "statefulset", sts.Name)
				return ctrl.Result{RequeueAfter: time.Minute}, nil
			}
		}

		var memJobs batchv1.JobList
		for _, sts := range stsList.Items {
			memJobName := fmt.Sprintf("%s-membership-%s-%d", action, sts.Name, replicas)
			var memJob batchv1.Job
			if err := r.Get(ctx, client.ObjectKey{Namespace: mda.Spec.Bitnami.Namespace, Name: memJobName}, &memJob); err != nil {
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}

			if memJob.Status.Failed > 0 {
				log.Error(fmt.Errorf("membership Job failed"), "membership Job encountered failures", "statefulset", sts.Name, "replicaIndex", replicas, "job", memJobName)
				foreground := metav1.DeletePropagationForeground
				_ = r.Delete(ctx, &memJob, &client.DeleteOptions{
					PropagationPolicy: &foreground,
				})
				return ctrl.Result{RequeueAfter: time.Minute}, nil
			}

			if memJob.Status.Succeeded < 1 {
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}

			memJobs.Items = append(memJobs.Items, memJob)
		}

		log.Info("membership Jobs completed successfully", "replicaIndex", replicas)
		for _, memJob := range memJobs.Items {
			foreground := metav1.DeletePropagationForeground
			_ = r.Delete(ctx, &memJob, &client.DeleteOptions{
				PropagationPolicy: &foreground,
			})
		}

		mda.Status.ScalingPhase = "ScalingDownDeletingReplica"
	case "ScalingDownDeletingReplica":
		replicas := mda.Status.TargetReplicaCount
		if err := r.patchBitnamiReplica(ctx, mda, replicas); err != nil {
			log.Error(err, "failed to delete bitnami replica")
			return ctrl.Result{RequeueAfter: time.Minute}, nil
		} else {
			log.Info("Replica scaling-down process finished")
			mda.Status.ScalingPhase = "Idle"
			mda.Status.TargetReplicaCount = -1
			mda.Status.LastScaleTime = metav1.Now()
		}
	default:
		log.Info("Unknown scaling phase; resetting to Idle", "scalingPhase", mda.Status.ScalingPhase)
		mda.Status.ScalingPhase = "Idle"
	}
	_ = r.Status().Update(ctx, mda)
	return ctrl.Result{}, nil
}

func (r *MongodAutoscalerReconciler) getDatanodeNodeIPs(ctx context.Context, mda *autoscalerv1alpha1.MongodAutoscaler) ([]string, error) {
	ns := mda.Spec.Bitnami.Namespace
	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.InNamespace(ns), client.MatchingLabels{
		"app.kubernetes.io/component": "shardsvr",
	}); err != nil {
		return nil, fmt.Errorf("failed to list datanode pods: %w", err)
	}

	ipSet := map[string]struct{}{}
	for _, pod := range podList.Items {
		if pod.Status.HostIP != "" {
			ipSet[pod.Status.HostIP] = struct{}{}
		}
	}

	ips := []string{}
	for ip := range ipSet {
		ips = append(ips, ip)
	}
	return ips, nil
}

func (r *MongodAutoscalerReconciler) currentBitnamiShards(ctx context.Context, mda *autoscalerv1alpha1.MongodAutoscaler) (appsv1.StatefulSetList, error) {
	var stsWholeList appsv1.StatefulSetList
	if err := r.List(ctx, &stsWholeList, &client.ListOptions{Namespace: mda.Spec.Bitnami.Namespace, LabelSelector: labels.SelectorFromSet(map[string]string{
		"app.kubernetes.io/name":     "mongodb-sharded",
		"app.kubernetes.io/instance": mda.Spec.Bitnami.ReleaseName,
	})}); err != nil {
		return appsv1.StatefulSetList{}, fmt.Errorf("failed to StatefulSet list: %w", err)
	}

	var stsList appsv1.StatefulSetList
	prefix := fmt.Sprintf("%s-shard", mda.Spec.Bitnami.ReleaseName)
	for _, sts := range stsWholeList.Items {
		if strings.HasPrefix(sts.Name, prefix) && strings.Contains(sts.Name, "-data") {
			stsList.Items = append(stsList.Items, sts)
		}
	}
	return stsList, nil
}

func (r *MongodAutoscalerReconciler) currentShardCount(ctx context.Context, mda *autoscalerv1alpha1.MongodAutoscaler) (int32, error) {
	stsList, err := r.currentBitnamiShards(ctx, mda)
	if err != nil {
		return 0, fmt.Errorf("failed to get current shards: %w", err)
	}
	return int32(len(stsList.Items)), nil
}

func (r *MongodAutoscalerReconciler) currentReplicaCount(ctx context.Context, mda *autoscalerv1alpha1.MongodAutoscaler) (int32, error) {
	stsList, err := r.currentBitnamiShards(ctx, mda)
	if err != nil {
		return 0, fmt.Errorf("failed to get current shards: %w", err)
	}

	count := int32(0)
	for _, sts := range stsList.Items {
		if count == 0 {
			count = *sts.Spec.Replicas
		} else if count != *sts.Spec.Replicas {
			return 0, fmt.Errorf("inconsistent replica counts across shards")
		}
	}

	return count, nil
}

func (r *MongodAutoscalerReconciler) isBalancerRunning(ctx context.Context, mda *autoscalerv1alpha1.MongodAutoscaler) (bool, error) {
	ns := mda.Spec.Bitnami.Namespace
	release := mda.Spec.Bitnami.ReleaseName
	secretName := release
	secretKey := "mongodb-root-password"
	host := fmt.Sprintf("%s.%s.svc.cluster.local:%d", release, ns, mda.Spec.Bitnami.ServicePort)

	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: secretName}, &secret); err != nil {
		return false, fmt.Errorf("failed to fetch secret %s: %w", secretName, err)
	}

	passwordBytes, exists := secret.Data[secretKey]
	if !exists {
		return false, fmt.Errorf("key '%s' not found in secret %s", secretKey, secretName)
	}
	password := string(passwordBytes)

	uri := fmt.Sprintf("mongodb://root:%s@%s/admin?authSource=admin", password, host)
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		return false, fmt.Errorf("failed to connect to MongoDB: %w", err)
	}
	defer client.Disconnect(ctx)

	adminDB := client.Database("admin")
	var result bson.M
	err = adminDB.RunCommand(ctx, bson.D{{Key: "balancerStatus", Value: 1}}).Decode(&result)
	if err != nil {
		return false, fmt.Errorf("failed to query balancer status: %w", err)
	}

	// Check the "inBalancerRound" field
	if inBalancerRound, ok := result["inBalancerRound"].(bool); ok {
		return inBalancerRound, nil
	}
	return false, fmt.Errorf("unexpected response format: %v", result)
}

func (r *MongodAutoscalerReconciler) checkShardConnectivity(ctx context.Context, mda *autoscalerv1alpha1.MongodAutoscaler, sts appsv1.StatefulSet) error {
	ns := mda.Spec.Bitnami.Namespace
	release := mda.Spec.Bitnami.ReleaseName
	secretName := release
	secretKey := "mongodb-root-password"
	port := mda.Spec.Bitnami.ServicePort
	host := fmt.Sprintf("%s-0.%s-headless.%s.svc.cluster.local:%d", sts.Name, release, ns, port)

	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: secretName}, &secret); err != nil {
		return fmt.Errorf("failed to fetch secret %s: %w", secretName, err)
	}

	passwordBytes, exists := secret.Data[secretKey]
	if !exists {
		return fmt.Errorf("key '%s' not found in secret %s", secretKey, secretName)
	}
	password := string(passwordBytes)

	uri := fmt.Sprintf("mongodb://root:%s@%s/admin?authSource=admin", password, host)
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		return fmt.Errorf("failed to connect to MongoDB: %w", err)
	}
	defer client.Disconnect(ctx)

	if err := client.Ping(ctx, nil); err != nil {
		return fmt.Errorf("failed to ping MongoDB: %w", err)
	}

	return nil
}

func (r *MongodAutoscalerReconciler) listShardDatabases(ctx context.Context, mda *autoscalerv1alpha1.MongodAutoscaler, idx int) ([]string, error) {
	ns := mda.Spec.Bitnami.Namespace
	release := mda.Spec.Bitnami.ReleaseName
	secretName := release
	secretKey := "mongodb-root-password"
	host := fmt.Sprintf("%s-shard%d-data-0.%s-headless.%s.svc.cluster.local:%d",
		release, idx, release, ns, mda.Spec.Bitnami.ServicePort)

	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: secretName}, &secret); err != nil {
		return nil, fmt.Errorf("failed to fetch secret %s: %w", secretName, err)
	}

	passwordBytes, exists := secret.Data[secretKey]
	if !exists {
		return nil, fmt.Errorf("key '%s' not found in secret %s", secretKey, secretName)
	}
	password := string(passwordBytes)

	uri := fmt.Sprintf("mongodb://root:%s@%s/admin?authSource=admin", password, host)
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		return nil, fmt.Errorf("failed to connect to MongoDB: %w", err)
	}
	defer client.Disconnect(ctx)

	adminDB := client.Database("admin")
	var result bson.M
	if err := adminDB.RunCommand(ctx, bson.D{{Key: "listDatabases", Value: 1}}).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to list databases: %w", err)
	}

	databases, ok := result["databases"].(primitive.A)
	if !ok {
		return nil, fmt.Errorf("unexpected format for databases: %v", result)
	}

	var dbList []string
	for _, db := range databases {
		if dbMap, ok := db.(primitive.M); ok {
			if name, ok := dbMap["name"].(string); ok {
				// Filter out system databases: "admin", "config", "local"
				if name != "admin" && name != "config" && name != "local" {
					dbList = append(dbList, name)
				}
			}
		}
	}

	return dbList, nil
}

func (r *MongodAutoscalerReconciler) createBitnamiShard(ctx context.Context, mda *autoscalerv1alpha1.MongodAutoscaler, shardIdx int) error {
	ns := mda.Spec.Bitnami.Namespace
	release := mda.Spec.Bitnami.ReleaseName

	// Find base shard StatefulSet to clone (prefer shard0)
	baseName := fmt.Sprintf("%s-shard0-data", release)
	var base appsv1.StatefulSet
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: baseName}, &base); err != nil {
		return fmt.Errorf("failed to load base shard %s: %w", baseName, err)
	}

	// Clone and mutate minimal fields
	clone := base.DeepCopy()
	clone.ResourceVersion = ""
	clone.UID = ""
	clone.Generation = 0
	clone.ManagedFields = nil
	clone.ObjectMeta = metav1.ObjectMeta{
		Name:      fmt.Sprintf("%s-shard%d-data", release, shardIdx),
		Namespace: ns,
		Labels:    map[string]string{},
	}
	for k, v := range base.Labels {
		clone.Labels[k] = v
	}

	// Adjust selector/template labels
	if clone.Spec.Selector == nil {
		clone.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{}}
	}
	if clone.Spec.Selector.MatchLabels == nil {
		clone.Spec.Selector.MatchLabels = map[string]string{}
	}
	if clone.Spec.Template.Labels == nil {
		clone.Spec.Template.Labels = map[string]string{}
	}
	if _, ok := clone.Spec.Template.Labels["shard"]; ok {
		clone.Spec.Template.Labels["shard"] = strconv.Itoa(shardIdx)
	}

	// Update envs in primary container
	if len(clone.Spec.Template.Spec.Containers) > 0 {
		c := &clone.Spec.Template.Spec.Containers[0]
		for i, e := range c.Env {
			switch e.Name {
			case "MONGODB_REPLICA_SET_NAME":
				c.Env[i].Value = fmt.Sprintf("%s-shard-%d", release, shardIdx)
			case "MONGODB_INITIAL_PRIMARY_HOST":
				c.Env[i].Value = fmt.Sprintf("%s-shard%d-data-0.%s-headless.%s.svc.cluster.local", release, shardIdx, release, ns)
			case "MONGODB_ADVERTISED_HOSTNAME":
				c.Env[i].Value = fmt.Sprintf("$(MONGODB_POD_NAME).%s-headless.%s.svc.cluster.local", release, ns)
			case "MONGODB_MONGOS_HOST":
				c.Env[i].Value = release
			case "MONGODB_MONGOS_PORT_NUMBER":
				c.Env[i].Value = fmt.Sprintf("%d", mda.Spec.Bitnami.ServicePort)
			}
		}
	}

	log.FromContext(ctx).Info("Creating StatefulSet", "statefulset", clone)

	if err := r.Create(ctx, clone); client.IgnoreAlreadyExists(err) != nil {
		return err
	}
	return nil
}

func (r *MongodAutoscalerReconciler) deleteBitnamiShard(ctx context.Context, mda *autoscalerv1alpha1.MongodAutoscaler, idx int) error {
	ns := mda.Spec.Bitnami.Namespace
	release := mda.Spec.Bitnami.ReleaseName
	stsName := fmt.Sprintf("%s-shard%d-data", release, idx)
	_ = r.Delete(ctx, &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: stsName, Namespace: ns}})
	return nil
}

func (r *MongodAutoscalerReconciler) createAddBitnamiShardJob(ctx context.Context, mda *autoscalerv1alpha1.MongodAutoscaler, shardIdx int) error {
	ns := mda.Spec.Bitnami.Namespace
	release := mda.Spec.Bitnami.ReleaseName
	secretName := release
	secretKey := "mongodb-root-password"
	host := release
	replSet := bitnamiReplicaSetName(release, shardIdx)
	shardHost := fmt.Sprintf("%s-shard%d-data-0.%s-headless.%s.svc.cluster.local:%d",
		release, shardIdx, release, ns, mda.Spec.Bitnami.ServicePort)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("addshard-%s", replSet),
			Namespace: ns,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptrI32(0),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:    "addshard",
						Image:   "mongo:6.0",
						Command: []string{"/bin/sh", "-c"},
						Env: []corev1.EnvVar{{
							Name: "MONGODB_ROOT_PASSWORD",
							ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
								Key:                  secretKey,
							}},
						}},
						Args: []string{fmt.Sprintf("mongosh --host %s -u root -p \"$MONGODB_ROOT_PASSWORD\" --authenticationDatabase admin --quiet --eval 'db.adminCommand({ addShard: \"%s/%s\" })'",
							host, replSet, shardHost),
						},
					}},
				},
			},
		},
	}
	if err := r.Create(ctx, job); client.IgnoreAlreadyExists(err) != nil {
		return err
	}
	return nil
}

func (r *MongodAutoscalerReconciler) createRemoveBitnamiShardJob(ctx context.Context, mda *autoscalerv1alpha1.MongodAutoscaler, shardIdx int) error {
	ns := mda.Spec.Bitnami.Namespace
	release := mda.Spec.Bitnami.ReleaseName
	secretName := release
	secretKey := "mongodb-root-password"
	host := release
	replSet := bitnamiReplicaSetName(release, shardIdx)
	// Build a mongosh script that loops calling removeShard until it reports state "completed" (drain finished),
	// or the shard is already gone (ShardNotFound), with a safe timeout.
	js := fmt.Sprintf(`
			const id = "%s";
			let attempts = 0;
			while (true) {
				const r = db.adminCommand({ removeShard: id });
				printjson(r);
				if (r.ok !== 1 && r.codeName !== "ShardNotFound") { quit(1); }
				if (r.state === "completed" || r.msg === "completed" || r.codeName === "ShardNotFound") { break; }
				attempts++;
				if (attempts > 3600) { print("timeout waiting for drain"); quit(1); }
				sleep(5000);
			}
			print("drain completed for "+id);
		`, replSet)

	// Deterministic job name (no UID suffix). Safe because we recreate only if absent.
	jobName := fmt.Sprintf("removeshard-%s", replSet)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: ns,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptrI32(0),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:    "removeshard",
						Image:   "mongo:6.0",
						Command: []string{"/bin/sh", "-c"},
						Env: []corev1.EnvVar{{
							Name: "MONGODB_ROOT_PASSWORD",
							ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
								Key:                  secretKey,
							}},
						}},
						Args: []string{fmt.Sprintf("mongosh --host %s -u root -p \"$MONGODB_ROOT_PASSWORD\" --authenticationDatabase admin --quiet --eval '%s'",
							host, js),
						},
					}},
				},
			},
		},
	}
	if err := r.Create(ctx, job); client.IgnoreAlreadyExists(err) != nil {
		return err
	}
	return nil
}

func (r *MongodAutoscalerReconciler) createDropDBJob(ctx context.Context, mda *autoscalerv1alpha1.MongodAutoscaler, shardIdx int, dbList []string) error {
	ns := mda.Spec.Bitnami.Namespace
	release := mda.Spec.Bitnami.ReleaseName
	secretName := release
	secretKey := "mongodb-root-password"
	host := fmt.Sprintf("%s-shard%d-data-0.%s-headless.%s.svc.cluster.local:%d",
		release, shardIdx, release, ns, mda.Spec.Bitnami.ServicePort)

	jobName := fmt.Sprintf("dropdb-shard%d", shardIdx)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: ns,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptrI32(0),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:    "dropdb",
						Image:   "mongo:6.0",
						Command: []string{"/bin/sh", "-c"},
						Env: []corev1.EnvVar{{
							Name: "MONGODB_ROOT_PASSWORD",
							ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
								Key:                  secretKey,
							}},
						}},
						Args: []string{fmt.Sprintf(
							"mongosh --host %s -u root -p \"$MONGODB_ROOT_PASSWORD\" --authenticationDatabase admin --quiet --eval '%s'",
							host, generateDropDBCommands(dbList)),
						},
					}},
				},
			},
		},
	}
	if err := r.Create(ctx, job); client.IgnoreAlreadyExists(err) != nil {
		return err
	}
	return nil
}

func (r *MongodAutoscalerReconciler) patchBitnamiReplica(ctx context.Context, mda *autoscalerv1alpha1.MongodAutoscaler, replicaCount int32) error {
	stsList, err := r.currentBitnamiShards(ctx, mda)
	if err != nil {
		return fmt.Errorf("failed to get current shards: %w", err)
	}

	for _, sts := range stsList.Items {
		// Patch the StatefulSet to update the replica count
		patch := client.MergeFrom(sts.DeepCopy())
		sts.Spec.Replicas = &replicaCount
		if err := r.Patch(ctx, &sts, patch); err != nil {
			return fmt.Errorf("failed to patch StatefulSet %s: %w", sts.Name, err)
		}
	}

	log.FromContext(ctx).Info("Patched Bitnami shard StatefulSets", "newReplicaCount", replicaCount)

	return nil
}

func (r *MongodAutoscalerReconciler) createReplicaMembershipJob(ctx context.Context, mda *autoscalerv1alpha1.MongodAutoscaler, sts appsv1.StatefulSet, replIdx int32, action string) error {
	ns := mda.Spec.Bitnami.Namespace
	release := mda.Spec.Bitnami.ReleaseName
	port := mda.Spec.Bitnami.ServicePort
	secretName := release
	secretKey := "mongodb-root-password"
	primary := fmt.Sprintf("%s-0.%s-headless.%s.svc.cluster.local:%d", sts.Name, release, ns, port)
	host := fmt.Sprintf("%s-%d.%s-headless.%s.svc.cluster.local:%d", sts.Name, replIdx, release, ns, port)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-membership-%s-%d", action, sts.Name, replIdx),
			Namespace: ns,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptrI32(0),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:    fmt.Sprintf("%s-membership", action),
						Image:   "mongo:6.0",
						Command: []string{"/bin/sh", "-c"},
						Env: []corev1.EnvVar{{
							Name: "MONGODB_ROOT_PASSWORD",
							ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
								Key:                  secretKey,
							}},
						}},
						Args: []string{fmt.Sprintf("mongosh --host %s -u root -p \"$MONGODB_ROOT_PASSWORD\" --authenticationDatabase admin --quiet --eval 'rs.%s(\"%s\")'",
							primary, action, host),
						},
					}},
				},
			},
		},
	}
	if err := r.Create(ctx, job); client.IgnoreAlreadyExists(err) != nil {
		return err
	}
	return nil
}

// func (r *MongodAutoscalerReconciler) highestBitnamiIndex(ctx context.Context, mda *autoscalerv1alpha1.MongodAutoscaler) int {
// 	shardNames := r.currentBitnamiShardNames(ctx, mda)

// 	maxIdx := -1
// 	release := mda.Spec.Bitnami.ReleaseName
// 	prefix := fmt.Sprintf("%s-shard", release)
// 	for _, n := range shardNames {
// 		if strings.HasPrefix(n, prefix) {
// 			rest := strings.TrimPrefix(n, prefix)
// 			// extract leading digits
// 			digits := 0
// 			for digits < len(rest) && rest[digits] >= '0' && rest[digits] <= '9' {
// 				digits++
// 			}
// 			if digits > 0 {
// 				if idx, err := strconv.Atoi(rest[:digits]); err == nil && idx > maxIdx {
// 					maxIdx = idx
// 				}
// 			}
// 		}
// 	}
// 	return maxIdx
// }

// SetupWithManager sets up the controller with the Manager.
func (r *MongodAutoscalerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&autoscalerv1alpha1.MongodAutoscaler{}).
		Named("mongodautoscaler").
		Owns(&appsv1.StatefulSet{}).
		Owns(&batchv1.Job{}).
		Complete(r)
}

func ptrI32(v int32) *int32 {
	return &v
}

func bitnamiReplicaSetName(release string, idx int) string {
	return fmt.Sprintf("%s-shard-%d", release, idx)
}

func generateDropDBCommands(dbList []string) string {
	var sb strings.Builder
	for i, dbName := range dbList {
		if i > 0 {
			sb.WriteString("; ")
		}
		sb.WriteString(fmt.Sprintf("db.getSiblingDB(\"%s\").dropDatabase()", dbName))
	}
	return sb.String()
}
