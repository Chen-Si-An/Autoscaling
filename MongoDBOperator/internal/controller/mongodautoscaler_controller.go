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
	"sort"
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

	min := mda.Spec.ScaleBounds.MinShards
	max := mda.Spec.ScaleBounds.MaxShards
	curr := int32(len(r.currentBitnamiShardNames(ctx, mda)))
	target := float64(mda.Spec.Policy.CpuTargetPercent)
	tol := float64(mda.Spec.Policy.TolerancePercent)
	cooldown := time.Duration(mda.Spec.Policy.CooldownSeconds) * time.Second

	if time.Since(mda.Status.LastScaleTime.Time) < cooldown {
		return ctrl.Result{RequeueAfter: 45 * time.Second}, nil
	}

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

		queryPrefix := fmt.Sprintf("%s-shard[0-9]+-data", mda.Spec.Bitnami.ReleaseName)
		queryNS := mda.Spec.Bitnami.Namespace
		avgCPU, err := prom.QueryAvgCPU(ctx, queryNS, queryPrefix, mda.Spec.Policy.Window)
		if err != nil {
			log.Error(err, "prometheus query failed")
			return ctrl.Result{RequeueAfter: time.Minute}, nil
		}

		switch {
		case avgCPU > target+tol && curr < max:
			mda.Status.ScalingPhase = "ScalingUpCreatingShard"
			mda.Status.TargetShardIdx = curr
			mda.Status.LastObservedCPU = fmt.Sprintf("%f", avgCPU)
			mda.Status.LastDesiredShards = curr + 1
			mda.Status.CurrentShardNames = r.currentBitnamiShardNames(ctx, mda)
			log.Info("Scaling UP shards", "cpu", avgCPU, "old", curr, "new", curr+1)
			_ = r.Status().Update(ctx, mda)
			return ctrl.Result{}, nil
		case avgCPU < target-tol && curr > min:
			mda.Status.ScalingPhase = "ScalingDownRemovingShard"
			mda.Status.TargetShardIdx = curr - 1
			mda.Status.LastObservedCPU = fmt.Sprintf("%f", avgCPU)
			mda.Status.LastDesiredShards = curr - 1
			mda.Status.CurrentShardNames = r.currentBitnamiShardNames(ctx, mda)
			log.Info("Scaling DOWN shards", "cpu", avgCPU, "old", curr, "new", curr-1)
			_ = r.Status().Update(ctx, mda)
			return ctrl.Result{}, nil
		default:
			log.Info("No shard scaling action", "cpu", avgCPU, "shards", curr)
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
		shardIdx := int(mda.Status.TargetShardIdx)

		expectedSTS := fmt.Sprintf("%s-shard%d-data", mda.Spec.Bitnami.ReleaseName, shardIdx)
		var shardSTS appsv1.StatefulSet
		if err := r.Get(ctx, client.ObjectKey{Namespace: mda.Spec.Bitnami.Namespace, Name: expectedSTS}, &shardSTS); err != nil {
			log.Info("Waiting for shard StatefulSet before addShard", "statefulset", expectedSTS)
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		if shardSTS.Status.ReadyReplicas == 0 {
			log.Info("Shard StatefulSet not ready yet; postponing addShard", "statefulset", expectedSTS)
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		replSet := r.bitnamiReplicaSetName(mda.Spec.Bitnami.ReleaseName, shardIdx)
		if err := r.createAddBitnamiShardJob(ctx, mda, replSet, shardIdx); err != nil {
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

		log.Info("Resharding completed; scaling-up process finished")
		mda.Status.ScalingPhase = "Idle"
		mda.Status.TargetShardIdx = -1
		mda.Status.LastScaleTime = metav1.Now()
		mda.Status.CurrentShardNames = r.currentBitnamiShardNames(ctx, mda)
	case "ScalingDownRemovingShard":
		shardIdx := int(mda.Status.TargetShardIdx)
		replSet := r.bitnamiReplicaSetName(mda.Spec.Bitnami.ReleaseName, shardIdx)

		// Ensure the Job exists (idempotent)
		if err := r.createRemoveBitnamiShardJob(ctx, mda, replSet); err != nil {
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

		log.Info("Scale-down process finished")
		mda.Status.ScalingPhase = "Idle"
		mda.Status.TargetShardIdx = -1
		mda.Status.LastScaleTime = metav1.Now()
		mda.Status.CurrentShardNames = r.currentBitnamiShardNames(ctx, mda)
	default:
		log.Info("Unknown scaling phase; resetting to Idle", "scalingPhase", mda.Status.ScalingPhase)
		mda.Status.ScalingPhase = "Idle"
	}
	_ = r.Status().Update(ctx, mda)
	return ctrl.Result{}, nil
}

func (r *MongodAutoscalerReconciler) bitnamiReplicaSetName(release string, idx int) string {
	return fmt.Sprintf("%s-shard-%d", release, idx)
}

func (r *MongodAutoscalerReconciler) currentBitnamiShardNames(ctx context.Context, mda *autoscalerv1alpha1.MongodAutoscaler) []string {
	var stsList appsv1.StatefulSetList
	_ = r.List(ctx, &stsList, &client.ListOptions{Namespace: mda.Spec.Bitnami.Namespace, LabelSelector: labels.SelectorFromSet(map[string]string{
		"app.kubernetes.io/name":     "mongodb-sharded",
		"app.kubernetes.io/instance": mda.Spec.Bitnami.ReleaseName,
	})})
	release := mda.Spec.Bitnami.ReleaseName
	prefixName := fmt.Sprintf("%s-shard", release)
	names := make([]string, 0, len(stsList.Items))
	for _, s := range stsList.Items {
		if strings.HasPrefix(s.Name, prefixName) && strings.Contains(s.Name, "-data") {
			names = append(names, s.Name)
		}
	}
	sort.Strings(names)
	return names
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

func (r *MongodAutoscalerReconciler) createAddBitnamiShardJob(ctx context.Context, mda *autoscalerv1alpha1.MongodAutoscaler, replSet string, shardIdx int) error {
	ns := mda.Spec.Bitnami.Namespace
	release := mda.Spec.Bitnami.ReleaseName
	secretName := release
	secretKey := "mongodb-root-password"
	host := release
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

func (r *MongodAutoscalerReconciler) createRemoveBitnamiShardJob(ctx context.Context, mda *autoscalerv1alpha1.MongodAutoscaler, replSet string) error {
	ns := mda.Spec.Bitnami.Namespace
	release := mda.Spec.Bitnami.ReleaseName
	secretName := release
	secretKey := "mongodb-root-password"
	host := release
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

func (r *MongodAutoscalerReconciler) deleteBitnamiShard(ctx context.Context, mda *autoscalerv1alpha1.MongodAutoscaler, idx int) error {
	ns := mda.Spec.Bitnami.Namespace
	release := mda.Spec.Bitnami.ReleaseName
	stsName := fmt.Sprintf("%s-shard%d-data", release, idx)
	_ = r.Delete(ctx, &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: stsName, Namespace: ns}})
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
