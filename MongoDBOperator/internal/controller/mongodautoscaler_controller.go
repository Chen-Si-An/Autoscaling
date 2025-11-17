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
	Scheme    *runtime.Scheme
	IsScaling int8
	AddShard  bool
}

// +kubebuilder:rbac:groups=autoscaler.mongodb.io,resources=mongodautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaler.mongodb.io,resources=mongodautoscalers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=autoscaler.mongodb.io,resources=mongodautoscalers/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
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

	full := mda.Spec.Bitnami.ReleaseName

	var stsList appsv1.StatefulSetList
	err := r.List(ctx, &stsList, &client.ListOptions{Namespace: mda.Spec.Bitnami.ReleaseNamespace, LabelSelector: labels.SelectorFromSet(map[string]string{
		"app.kubernetes.io/name":     "mongodb-sharded",
		"app.kubernetes.io/instance": mda.Spec.Bitnami.ReleaseName,
	})})
	if err != nil {
		log.Error(err, "failed to list bitnami shard StatefulSets")
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	currShards := len(stsList.Items)
	shardNames := make([]string, 0, currShards)
	for _, s := range stsList.Items {
		shardNames = append(shardNames, s.Name)
	}
	prefixName := fmt.Sprintf("%s-shard", full)
	filtered := make([]string, 0, len(shardNames))
	for _, n := range shardNames {
		if strings.HasPrefix(n, prefixName) && strings.Contains(n, "-data") {
			filtered = append(filtered, n)
		}
	}
	shardNames = filtered
	currShards = len(filtered)

	prom, err := promclient.NewPromClient(mda.Spec.Prometheus.URL)
	if err != nil {
		log.Error(err, "failed to init Prometheus client")
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	queryPrefix := fmt.Sprintf("%s-shard[0-9]+-data", full)
	queryNS := mda.Spec.Bitnami.ReleaseNamespace
	avgCPU, err := prom.QueryAvgCPU(ctx, queryNS, queryPrefix, mda.Spec.Policy.Window)
	if err != nil {
		log.Error(err, "prometheus query failed")
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	min := int32(mda.Spec.ScaleBounds.MinShards)
	max := int32(mda.Spec.ScaleBounds.MaxShards)
	curr := int32(currShards)
	target := float64(mda.Spec.Policy.CpuTargetPercent)
	tol := float64(mda.Spec.Policy.TolerancePercent)
	cooldown := time.Duration(mda.Spec.Policy.CooldownSeconds) * time.Second

	if time.Since(mda.Status.LastScaleTime.Time) < cooldown {
		return ctrl.Result{RequeueAfter: 45 * time.Second}, nil
	}

	desired := curr
	if r.IsScaling == 0 {
		switch {
		case avgCPU > target+tol && curr < max:
			desired = curr + 1
			r.IsScaling = 1
			r.AddShard = true
			log.Info("Scaling UP shards", "cpu", avgCPU, "old", curr, "new", desired)
		case avgCPU < target-tol && curr > min:
			desired = curr - 1
			r.IsScaling = 2
			log.Info("Scaling DOWN shards", "cpu", avgCPU, "old", curr, "new", desired)
		default:
			log.Info("No shard scaling action", "cpu", avgCPU, "shards", curr)
		}
	}

	if r.IsScaling > 0 {
		if r.IsScaling == 1 { // scale up
			var nextIdx int
			if r.AddShard {
				nextIdx = r.highestBitnamiIndex(shardNames, mda.Spec.Bitnami.ReleaseName) + 1
				if mda.Spec.Bitnami.ControllerManaged {
					if err := r.createBitnamiShard(ctx, mda, nextIdx); err != nil {
						log.Error(err, "failed to create bitnami shard resources")
						return ctrl.Result{RequeueAfter: time.Minute}, nil
					} else {
						r.AddShard = false
					}
				}
			} else {
				nextIdx = r.highestBitnamiIndex(shardNames, mda.Spec.Bitnami.ReleaseName)
			}
			// Only run addShard after the new shard StatefulSet exists and has pods ready.
			expectedSTS := fmt.Sprintf("%s-shard%d-data", full, nextIdx)
			var shardSTS appsv1.StatefulSet
			if err := r.Get(ctx, client.ObjectKey{Namespace: mda.Spec.Bitnami.ReleaseNamespace, Name: expectedSTS}, &shardSTS); err != nil {
				log.Info("Waiting for shard StatefulSet before addShard", "statefulset", expectedSTS)
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			} else if shardSTS.Status.ReadyReplicas > 0 {
				replSet := r.bitnamiReplicaSetFullName(full, nextIdx)
				if err := r.createAddShardJobBitnami(ctx, mda, replSet, nextIdx); err != nil {
					log.Error(err, "failed to create addShard Job (bitnami)")
					return ctrl.Result{RequeueAfter: time.Minute}, nil
				} else {
					jobName := fmt.Sprintf("addshard-%s", replSet)
					var addJob batchv1.Job
					if err := r.Get(ctx, client.ObjectKey{Namespace: mda.Spec.Bitnami.ReleaseNamespace, Name: jobName}, &addJob); err == nil {
						if addJob.Status.Succeeded > 0 {
							log.Info("addShard Job completed successfully", "replSet", replSet, "job", jobName)
							foreground := metav1.DeletePropagationForeground
							_ = r.Delete(ctx, &addJob, &client.DeleteOptions{
								PropagationPolicy: &foreground,
							})
							r.IsScaling = 0
						} else {
							return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
						}
					} else {
						return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
					}
				}
			} else {
				log.Info("Shard StatefulSet not ready yet; postponing addShard", "statefulset", expectedSTS)
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}
		} else { // scale down
			sort.Strings(shardNames)
			idx := r.highestBitnamiIndex(shardNames, mda.Spec.Bitnami.ReleaseName)
			if idx >= 0 {
				full := mda.Spec.Bitnami.ReleaseName
				shardRepl := r.bitnamiReplicaSetFullName(full, idx)
				// Ensure the Job exists (idempotent). Name no longer includes UID.
				if err := r.createRemoveShardJobBitnami(ctx, mda, shardRepl); err != nil {
					log.Error(err, "failed to create removeShard Job (bitnami)")
					return ctrl.Result{RequeueAfter: time.Minute}, nil
				}
				// Wait for Job completion (which indicates draining is completed).
				jobName := fmt.Sprintf("removeshard-%s", shardRepl)
				var rmJob batchv1.Job
				if err := r.Get(ctx, client.ObjectKey{Namespace: mda.Spec.Bitnami.ReleaseNamespace, Name: jobName}, &rmJob); err == nil {
					if rmJob.Status.Succeeded < 1 {
						log.Info("Waiting for shard drain to complete before deleting resources", "replSet", shardRepl, "job", jobName)
						return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
					}
				} else {
					log.Info("Waiting for removeShard Job to be created", "job", jobName)
					return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
				}
				// Draining completed; run a dropDB job on the shard's primary to clean up local data before deletion
				dbName := "ycsb"
				dropJobName := fmt.Sprintf("dropdb-%s-shard%d", dbName, idx)
				var dropJob batchv1.Job
				if err := r.Get(ctx, client.ObjectKey{Namespace: mda.Spec.Bitnami.ReleaseNamespace, Name: dropJobName}, &dropJob); err != nil {
					// Create if not exists
					if err := r.createDropDBJobBitnami(ctx, mda, idx, dbName); err != nil {
						log.Error(err, "failed to create dropDB Job (bitnami)", "job", dropJobName)
						return ctrl.Result{RequeueAfter: time.Minute}, nil
					} else {
						log.Info("dropDB Job created successfully", "job", dropJobName)
					}
				}
				if dropJob.Status.Succeeded < 1 {
					log.Info("Waiting for dropDB job to complete before deleting resources", "job", dropJobName)
					return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
				}

				// DropDB completed; now it's safe to delete shard resources if controller-managed
				if mda.Spec.Bitnami.ControllerManaged {
					if err := r.deleteBitnamiShard(ctx, mda, idx); err != nil {
						log.Error(err, "failed to delete bitnami shard resources")
						return ctrl.Result{RequeueAfter: time.Minute}, nil
					}

					// Clean up RemoveShard Job and DropDB Job if successful
					foreground := metav1.DeletePropagationForeground
					if rmJob.Status.Succeeded > 0 {
						log.Info("Cleaning up RemoveShard Job", "job", rmJob.Name)
						_ = r.Delete(ctx, &rmJob, &client.DeleteOptions{
							PropagationPolicy: &foreground,
						})
					}
					if dropJob.Status.Succeeded > 0 {
						log.Info("Cleaning up DropDB Job", "job", dropJob.Name)
						_ = r.Delete(ctx, &dropJob, &client.DeleteOptions{
							PropagationPolicy: &foreground,
						})
						r.IsScaling = 0
					}
				}
			}
		}

		mda.Status.LastScaleTime = metav1.Now()
		mda.Status.LastObservedCPU = fmt.Sprintf("%f", avgCPU)
		mda.Status.LastDesiredShards = desired
		mda.Status.CurrentShardNames = r.currentShardNamesBitnami(ctx, mda)
		_ = r.Status().Update(ctx, mda)
	} else {
		mda.Status.LastObservedCPU = fmt.Sprintf("%f", avgCPU)
		if mda.Spec.Bitnami != nil {
			mda.Status.CurrentShardNames = r.currentShardNamesBitnami(ctx, mda)
		}
		_ = r.Status().Update(ctx, mda)
	}

	return ctrl.Result{RequeueAfter: 45 * time.Second}, nil
}

func (r *MongodAutoscalerReconciler) highestBitnamiIndex(names []string, release string) int {
	maxIdx := -1
	full := release
	prefix := fmt.Sprintf("%s-shard", full)
	for _, n := range names {
		if strings.HasPrefix(n, prefix) {
			rest := strings.TrimPrefix(n, prefix)
			// extract leading digits
			digits := 0
			for digits < len(rest) && rest[digits] >= '0' && rest[digits] <= '9' {
				digits++
			}
			if digits > 0 {
				if idx, err := strconv.Atoi(rest[:digits]); err == nil && idx > maxIdx {
					maxIdx = idx
				}
			}
		}
	}
	return maxIdx
}

func (r *MongodAutoscalerReconciler) bitnamiReplicaSetFullName(full string, idx int) string {
	return fmt.Sprintf("%s-shard-%d", full, idx)
}

func (r *MongodAutoscalerReconciler) currentShardNamesBitnami(ctx context.Context, mda *autoscalerv1alpha1.MongodAutoscaler) []string {
	var stsList appsv1.StatefulSetList
	_ = r.List(ctx, &stsList, &client.ListOptions{Namespace: mda.Spec.Bitnami.ReleaseNamespace, LabelSelector: labels.SelectorFromSet(map[string]string{
		"app.kubernetes.io/name":     "mongodb-sharded",
		"app.kubernetes.io/instance": mda.Spec.Bitnami.ReleaseName,
	})})
	full := mda.Spec.Bitnami.ReleaseName
	prefixName := fmt.Sprintf("%s-shard", full)
	names := make([]string, 0, len(stsList.Items))
	for _, s := range stsList.Items {
		if strings.HasPrefix(s.Name, prefixName) && strings.Contains(s.Name, "-data") {
			names = append(names, s.Name)
		}
	}
	sort.Strings(names)
	return names
}

func (r *MongodAutoscalerReconciler) createBitnamiShard(ctx context.Context, mda *autoscalerv1alpha1.MongodAutoscaler, idx int) error {
	ns := mda.Spec.Bitnami.ReleaseNamespace
	release := mda.Spec.Bitnami.ReleaseName
	full := release

	// Find base shard StatefulSet to clone (prefer shard0)
	baseName := fmt.Sprintf("%s-shard0-data", full)
	var base appsv1.StatefulSet
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: baseName}, &base); err != nil {
		// Fallback to highest existing shard as template
		var stsList appsv1.StatefulSetList
		if err2 := r.List(ctx, &stsList, &client.ListOptions{Namespace: ns, LabelSelector: labels.SelectorFromSet(map[string]string{
			"app.kubernetes.io/name":     "mongodb-sharded",
			"app.kubernetes.io/instance": release,
		})}); err2 != nil || len(stsList.Items) == 0 {
			return fmt.Errorf("cannot find base shard StatefulSet: %w", err)
		}
		names := make([]string, 0, len(stsList.Items))
		for _, it := range stsList.Items {
			names = append(names, it.Name)
		}
		hi := r.highestBitnamiIndex(names, release)
		if hi < 0 {
			return fmt.Errorf("no matching bitnami shard StatefulSet found to clone")
		}
		name := fmt.Sprintf("%s-shard%d-data", full, hi)
		if err3 := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &base); err3 != nil {
			return fmt.Errorf("failed to load base shard %s: %w", name, err3)
		}
	}

	// Clone and mutate minimal fields
	clone := base.DeepCopy()
	clone.ResourceVersion = ""
	clone.UID = ""
	clone.Generation = 0
	clone.ManagedFields = nil
	clone.ObjectMeta = metav1.ObjectMeta{
		Name:      fmt.Sprintf("%s-shard%d-data", full, idx),
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
		clone.Spec.Template.Labels["shard"] = strconv.Itoa(idx)
	}

	// Update envs in primary container
	if len(clone.Spec.Template.Spec.Containers) > 0 {
		c := &clone.Spec.Template.Spec.Containers[0]
		for i, e := range c.Env {
			switch e.Name {
			case "MONGODB_REPLICA_SET_NAME":
				c.Env[i].Value = fmt.Sprintf("%s-shard-%d", full, idx)
			case "MONGODB_INITIAL_PRIMARY_HOST":
				c.Env[i].Value = fmt.Sprintf("%s-shard%d-data-0.%s-headless.%s.svc.cluster.local", full, idx, full, ns)
			case "MONGODB_ADVERTISED_HOSTNAME":
				c.Env[i].Value = fmt.Sprintf("$(MONGODB_POD_NAME).%s-headless.%s.svc.cluster.local", full, ns)
			case "MONGODB_MONGOS_HOST":
				c.Env[i].Value = full
			case "MONGODB_MONGOS_PORT_NUMBER":
				c.Env[i].Value = fmt.Sprintf("%d", mda.Spec.Target.ServicePort)
			}
		}
	}

	log.FromContext(ctx).Info("Creating StatefulSet", "statefulset", clone)

	if err := r.Create(ctx, clone); client.IgnoreAlreadyExists(err) != nil {
		return err
	}
	return nil
}

func (r *MongodAutoscalerReconciler) createAddShardJobBitnami(ctx context.Context, mda *autoscalerv1alpha1.MongodAutoscaler, replSet string, idx int) error {
	ns := mda.Spec.Bitnami.ReleaseNamespace
	full := mda.Spec.Bitnami.ReleaseName
	host := fmt.Sprintf("%s-shard%d-data-0.%s-headless.%s.svc.cluster.local:%d",
		full, idx, full, ns, mda.Spec.Target.ServicePort)

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
							Name: "MONGODB_URI",
							ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: mda.Spec.Router.SecretRef.Name},
								Key:                  "uri",
							}},
						}},
						Args: []string{fmt.Sprintf("mongosh \"$MONGODB_URI\" --quiet --eval 'db.adminCommand({ addShard: \"%s/%s\" })'", replSet, host)},
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

func (r *MongodAutoscalerReconciler) createRemoveShardJobBitnami(ctx context.Context, mda *autoscalerv1alpha1.MongodAutoscaler, replSet string) error {
	ns := mda.Spec.Bitnami.ReleaseNamespace
	// Derive shard index from replSet (expected format: shard<N>) for optional ID resolution
	shardIdx := -1
	if strings.Contains(replSet, "-shard-") {
		parts := strings.Split(replSet, "-shard-")
		if len(parts) == 2 {
			if n, err := strconv.Atoi(parts[1]); err == nil {
				shardIdx = n
			}
		}
	}

	// Build a mongosh script that resolves the target shard id and then loops
	// calling removeShard until it reports state "completed" (drain finished),
	// or the shard is already gone (ShardNotFound), with a safe timeout.
	js := ""
	if shardIdx >= 0 {
		js = fmt.Sprintf(`
			const list = db.adminCommand({ listShards: 1 });
			if (!list.ok) { printjson(list); quit(1); }
			const shards = list.shards || [];
			const idx = %d;
			function matchHost(h) {
				if (!h) return false;
				return h.includes("-shard"+idx+"-data");
			}
			const found = shards.find(s => matchHost(s.host));
			const id = found ? (found._id || found.id) : "%s";
			print("Using shard id: "+id);
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
		`, shardIdx, replSet)
	} else {
		js = fmt.Sprintf(`
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
	}

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
							Name: "MONGODB_URI",
							ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: mda.Spec.Router.SecretRef.Name},
								Key:                  "uri",
							}},
						}},
						Args: []string{fmt.Sprintf("mongosh \"$MONGODB_URI\" --quiet --eval '%s'", js)},
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

func (r *MongodAutoscalerReconciler) createDropDBJobBitnami(ctx context.Context, mda *autoscalerv1alpha1.MongodAutoscaler, idx int, dbName string) error {
	ns := mda.Spec.Bitnami.ReleaseNamespace
	full := mda.Spec.Bitnami.ReleaseName
	host := fmt.Sprintf("%s-shard%d-data-0.%s-headless.%s.svc.cluster.local:%d",
		full, idx, full, ns, mda.Spec.Target.ServicePort)

	// Determine the Secret name/key for the root password. Default to Bitnami chart's convention
	// but allow override of Secret name via spec.bitnami.rootPasswordSecretRef (key fixed by convention).
	secretName := full
	secretKey := "mongodb-root-password"

	jobName := fmt.Sprintf("dropdb-%s-shard%d", dbName, idx)
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
							"mongosh --host %s -u root -p \"$MONGODB_ROOT_PASSWORD\" --authenticationDatabase admin --quiet --eval 'db.getSiblingDB(\"%s\").dropDatabase();'",
							host, dbName)},
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
	ns := mda.Spec.Bitnami.ReleaseNamespace
	full := mda.Spec.Bitnami.ReleaseName
	stsName := fmt.Sprintf("%s-shard%d-data", full, idx)
	_ = r.Delete(ctx, &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: stsName, Namespace: ns}})
	return nil
}

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
