/*
Copyright 2021 The Kubernetes Authors.

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

package util

import (
	"context"
	"encoding/json"
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog"

	"sigs.k8s.io/multi-tenancy/incubator/virtualcluster/experiment/pkg/apis/cluster/v1alpha4"
	internalcache "sigs.k8s.io/multi-tenancy/incubator/virtualcluster/experiment/pkg/scheduler/cache"
	"sigs.k8s.io/multi-tenancy/incubator/virtualcluster/experiment/pkg/scheduler/constants"
	"sigs.k8s.io/multi-tenancy/incubator/virtualcluster/pkg/apis/tenancy/v1alpha1"
	syncerconst "sigs.k8s.io/multi-tenancy/incubator/virtualcluster/pkg/syncer/constants"
	"sigs.k8s.io/multi-tenancy/incubator/virtualcluster/pkg/syncer/conversion"
	utilconst "sigs.k8s.io/multi-tenancy/incubator/virtualcluster/pkg/util/constants"
)

func GetClientFromSecret(metaClient clientset.Interface, name, namespace string) (*clientset.Clientset, error) {
	adminKubeConfigSecret, err := metaClient.CoreV1().Secrets(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get secret %s/%s from meta cluster: %v", namespace, name, err)
	}
	adminKubeConfigBytes, ok := adminKubeConfigSecret.Data[constants.KubeconfigAdminSecretName]
	if !ok {
		return nil, fmt.Errorf("failed to get kubeconfig from secret %s/%s", namespace, name)
	}

	restConfig, err := clientcmd.RESTConfigFromKubeConfig(adminKubeConfigBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to create restconfig from kubeconfig %v", err)
	}
	client, err := clientset.NewForConfig(restclient.AddUserAgent(restConfig, constants.SchedulerUserAgent))
	if err != nil {
		return nil, fmt.Errorf("failed to create new client from restconfig %v", err)
	}
	return client, nil
}

func GetSuperClusterID(client clientset.Interface) (string, error) {
	cfg, err := client.CoreV1().ConfigMaps("kube-system").Get(context.TODO(), utilconst.SuperClusterInfoCfgMap, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get super cluster info configmap in kube-system")
	}
	id, ok := cfg.Data[utilconst.SuperClusterIDKey]
	if !ok {
		return "", fmt.Errorf("failed to get super cluster id from the supercluster-info configmap in kube-system")
	}
	return id, nil
}

func getTotalNodeCapacity(nodelist *v1.NodeList) v1.ResourceList {
	total := v1.ResourceList{
		v1.ResourceCPU:    resource.MustParse("0"),
		v1.ResourceMemory: resource.MustParse("0"),
	}
	for _, each := range nodelist.Items {
		cur := total[v1.ResourceCPU]
		cur.Add(each.Status.Capacity[v1.ResourceCPU])
		total[v1.ResourceCPU] = cur

		cur = total[v1.ResourceMemory]
		cur.Add(each.Status.Capacity[v1.ResourceMemory])
		total[v1.ResourceMemory] = cur
	}
	return total
}

func GetSuperClusterCapacity(client clientset.Interface) (v1.ResourceList, error) {
	nodelist, err := client.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get node from super cluster %v", err)
	}
	// TODO: we need leave some headroom before reporting the capacity to tolerate node failures.
	return getTotalNodeCapacity(nodelist), nil
}

func SyncSuperClusterState(metaClient clientset.Interface, super *v1alpha4.Cluster, cache internalcache.Cache) error {
	client, err := GetClientFromSecret(metaClient, super.Name, super.Namespace)
	if err != nil {
		return fmt.Errorf("failed to get client for super cluster %s/%s: %v", super.Namespace, super.Name, err)
	}
	id, err := GetSuperClusterID(client)
	if err != nil {
		return fmt.Errorf("failed to get cluster id from super cluster: %v", err)
	}
	capacity, err := GetSuperClusterCapacity(client)
	if err != nil {
		return fmt.Errorf("failed to get cluster capacity from super cluster: %v", err)
	}
	var labels map[string]string
	if super.GetLabels() != nil {
		labels = make(map[string]string)
		for k, v := range super.GetLabels() {
			labels[k] = v
		}
	}
	klog.Infof("added cluster %s in cache", id)
	// TODO: we need to check if the cluster has been added, if so, we need to UPDATE the cluster
	if err := cache.AddCluster(internalcache.NewCluster(id, labels, capacity)); err != nil {
		return fmt.Errorf("failed to add cluster to cache: %s", id)
	}
	return nil
}

func getMaxQuota(quotalist *v1.ResourceQuotaList) v1.ResourceList {
	quota := v1.ResourceList{
		v1.ResourceCPU:    resource.MustParse("0"),
		v1.ResourceMemory: resource.MustParse("0"),
	}
	for _, each := range quotalist.Items {
		// for now, we ignore quotascope and scopeselector
		cpu, ok := each.Spec.Hard[v1.ResourceCPU]
		if ok {
			cur := quota[v1.ResourceCPU]
			if cur.Cmp(cpu) == -1 {
				quota[v1.ResourceCPU] = cpu
			}
		}
		mem, ok := each.Spec.Hard[v1.ResourceMemory]
		if ok {
			cur := quota[v1.ResourceMemory]
			if cur.Cmp(mem) == -1 {
				quota[v1.ResourceMemory] = mem
			}
		}
	}
	return quota
}

// GetNamespaceQuota returns the namespace quota for cpu and memory resouces.
// If there are multiple quota resources available, the largest quota is chosen.
func GetNamespaceQuota(client clientset.Interface, namespace string) (v1.ResourceList, error) {
	quotalist, err := client.CoreV1().ResourceQuotas(namespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get quota from namespace %s: %v", namespace, err)
	}
	return getMaxQuota(quotalist), nil
}

func GetPodRequirements(pod *v1.Pod) v1.ResourceList {
	request := v1.ResourceList{
		v1.ResourceCPU:    resource.MustParse("0"),
		v1.ResourceMemory: resource.MustParse("0"),
	}
	// We skip initcontainers for now
	for _, each := range pod.Spec.Containers {
		if each.Resources.Requests != nil {
			cpu, ok := each.Resources.Requests[v1.ResourceCPU]
			if ok {
				cur := request[v1.ResourceCPU]
				cur.Add(cpu)
				request[v1.ResourceCPU] = cur
			}
			mem, ok := each.Resources.Requests[v1.ResourceMemory]
			if ok {
				cur := request[v1.ResourceMemory]
				cur.Add(mem)
				request[v1.ResourceMemory] = cur
			}
		}
	}
	return request
}

func parseSlice(slice map[string]string) (v1.ResourceList, error) {
	quotaslice := utilconst.DefaultNamespaceSlice

	if val, ok := slice[string(v1.ResourceCPU)]; ok {
		quotaslice[v1.ResourceCPU] = resource.MustParse(val)
	} else {
		return nil, fmt.Errorf("wrong slice CPU format %v", slice)
	}

	if val, ok := slice[string(v1.ResourceMemory)]; ok {
		quotaslice[v1.ResourceMemory] = resource.MustParse(val)
	} else {
		return nil, fmt.Errorf("wrong slice Memory format %v", slice)
	}
	return quotaslice, nil
}

func SyncVirtualClusterState(metaClient clientset.Interface, vc *v1alpha1.VirtualCluster, cache internalcache.Cache) error {
	clustername := conversion.ToClusterKey(vc)
	client, err := GetClientFromSecret(metaClient, syncerconst.KubeconfigAdminSecretName, clustername)
	if err != nil {
		return fmt.Errorf("failed to get client for virtual cluster %s/%s: %v", vc.Namespace, vc.Name, err)
	}
	nslist, err := client.CoreV1().Namespaces().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to get namespaces from virtual cluster %s/%s: %v", vc.Namespace, vc.Name, err)
	}
	for _, each := range nslist.Items {
		klog.Infof("attempt to add namespace %s in cache", each.Name)

		quota, err := GetNamespaceQuota(client, each.Name)
		if err != nil {
			return fmt.Errorf("failed in %s/%s: %v", vc.Namespace, vc.Name, err)
		}
		cpu := quota[v1.ResourceCPU]
		mem := quota[v1.ResourceMemory]

		val, ok := each.GetAnnotations()[utilconst.LabelScheduledPlacements]
		if cpu.IsZero() && mem.IsZero() {
			if ok {
				// TODO: we may need to clear the schedule.
			}
			continue
		}

		if !ok {
			// to be scheduled, skip
			continue
		}
		placements := make(map[string]int)
		if err = json.Unmarshal([]byte(val), &placements); err != nil {
			return fmt.Errorf("unknown format %s of key %s, cluster %s, ns %s: %v", val, utilconst.LabelScheduledPlacements, vc.Name, each.Name, err)
		}

		var quotaSlice v1.ResourceList
		if val, ok = each.GetAnnotations()[utilconst.LabelNamespaceSlice]; ok {
			slice := make(map[string]string)
			if err = json.Unmarshal([]byte(val), &slice); err != nil {
				return fmt.Errorf("unknown format %s of key %s, cluster %s, ns %s: %v", val, utilconst.LabelNamespaceSlice, vc.Name, each.Name, err)
			}
			quotaSlice, err = parseSlice(slice)
			if err != nil {
				return fmt.Errorf("wrong slice format:%v", err)
			}
		} else {
			quotaSlice = utilconst.DefaultNamespaceSlice
		}
		total, _ := internalcache.GetNumSlices(quota, quotaSlice)
		numSched := 0
		schedule := make([]*internalcache.Placement, 0)
		for k, v := range placements {
			numSched = numSched + v
			schedule = append(schedule, internalcache.NewPlacement(k, v))
		}
		if total != numSched {
			fmt.Errorf("num of slices %d does not match num of sched slices %d", total, numSched)
		}

		var labels map[string]string
		if each.GetLabels() != nil {
			labels = make(map[string]string)
			for k, v := range each.GetLabels() {
				labels[k] = v
			}
		}
		cNamespace := internalcache.NewNamespace(clustername, each.Name, labels, quota, quotaSlice, schedule)
		// TODO: we need to check if the namespace has been added, if so, we need to UPDATE the namespace.
		if err := cache.AddNamespace(cNamespace); err != nil {
			return fmt.Errorf("failed to add namespace to cache: %s/%s", clustername, each.Name)
		}

		// continue to check the Pods in the namespace that use the quota
		podlist, err := client.CoreV1().Pods(each.Name).List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("failed to list pods in namespace %s in cluster %s", each.Name, clustername)
		}
		for _, pod := range podlist.Items {
			supercluster, ok := pod.GetAnnotations()[utilconst.LabelScheduledCluster]
			if !ok {
				continue
			}
			if _, ok := placements[supercluster]; !ok {
				// TODO: Pod scheduling result is inconsistent, we need to delete the Pod or send warnings.
				continue
			}
			cPod := internalcache.NewPod(clustername, each.Name, pod.Name, string(pod.UID), supercluster, GetPodRequirements(&pod))
			// TODO: we need to check if the pod has been added, if so, we need to UPDATE the pod.
			if err := cache.AddPod(cPod); err != nil {
				return fmt.Errorf("failed to add pod to cache: %s/%s/%s", clustername, each.Name, pod.Name)
			}
		}
	}
	return nil
}
