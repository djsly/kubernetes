/*
Copyright 2017 The Kubernetes Authors.

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

package scheduling

import (
	"fmt"
	"strings"
	"time"

	"k8s.io/client-go/tools/cache"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	schedulerapi "k8s.io/api/scheduling/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/pkg/apis/scheduling"
	"k8s.io/kubernetes/test/e2e/framework"
	e2elog "k8s.io/kubernetes/test/e2e/framework/log"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	"k8s.io/kubernetes/test/e2e/framework/replicaset"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	// ensure libs have a chance to initialize
	_ "github.com/stretchr/testify/assert"
)

type priorityPair struct {
	name  string
	value int32
}

var _ = SIGDescribe("SchedulerPreemption [Serial]", func() {
	var cs clientset.Interface
	var nodeList *corev1.NodeList
	var ns string
	f := framework.NewDefaultFramework("sched-preemption")

	lowPriority, mediumPriority, highPriority := int32(1), int32(100), int32(1000)
	lowPriorityClassName := f.BaseName + "-low-priority"
	mediumPriorityClassName := f.BaseName + "-medium-priority"
	highPriorityClassName := f.BaseName + "-high-priority"
	priorityPairs := []priorityPair{
		{name: lowPriorityClassName, value: lowPriority},
		{name: mediumPriorityClassName, value: mediumPriority},
		{name: highPriorityClassName, value: highPriority},
	}

	ginkgo.AfterEach(func() {
		for _, pair := range priorityPairs {
			cs.SchedulingV1().PriorityClasses().Delete(pair.name, metav1.NewDeleteOptions(0))
		}
	})

	ginkgo.BeforeEach(func() {
		cs = f.ClientSet
		ns = f.Namespace.Name
		nodeList = &corev1.NodeList{}
		for _, pair := range priorityPairs {
			_, err := f.ClientSet.SchedulingV1().PriorityClasses().Create(&schedulerapi.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: pair.name}, Value: pair.value})
			gomega.Expect(err == nil || errors.IsAlreadyExists(err)).To(gomega.Equal(true))
		}

		e2enode.WaitForTotalHealthy(cs, time.Minute)
		masterNodes, nodeList = framework.GetMasterAndWorkerNodesOrDie(cs)

		err := framework.CheckTestingNSDeletedExcept(cs, ns)
		framework.ExpectNoError(err)
	})

	// This test verifies that when a higher priority pod is created and no node with
	// enough resources is found, scheduler preempts a lower priority pod to schedule
	// the high priority pod.
	ginkgo.It("validates basic preemption works", func() {
		var podRes corev1.ResourceList
		// Create one pod per node that uses a lot of the node's resources.
		ginkgo.By("Create pods that use 60% of node resources.")
		pods := make([]*corev1.Pod, len(nodeList.Items))
		for i, node := range nodeList.Items {
			cpuAllocatable, found := node.Status.Allocatable["cpu"]
			gomega.Expect(found).To(gomega.Equal(true))
			milliCPU := cpuAllocatable.MilliValue() * 40 / 100
			memAllocatable, found := node.Status.Allocatable["memory"]
			gomega.Expect(found).To(gomega.Equal(true))
			memory := memAllocatable.Value() * 60 / 100
			podRes = corev1.ResourceList{}
			podRes[corev1.ResourceCPU] = *resource.NewMilliQuantity(int64(milliCPU), resource.DecimalSI)
			podRes[corev1.ResourceMemory] = *resource.NewQuantity(int64(memory), resource.BinarySI)

			// make the first pod low priority and the rest medium priority.
			priorityName := mediumPriorityClassName
			if i == 0 {
				priorityName = lowPriorityClassName
			}
			pods[i] = createPausePod(f, pausePodConfig{
				Name:              fmt.Sprintf("pod%d-%v", i, priorityName),
				PriorityClassName: priorityName,
				Resources: &corev1.ResourceRequirements{
					Requests: podRes,
				},
			})
			e2elog.Logf("Created pod: %v", pods[i].Name)
		}
		ginkgo.By("Wait for pods to be scheduled.")
		for _, pod := range pods {
			framework.ExpectNoError(e2epod.WaitForPodRunningInNamespace(cs, pod))
		}

		ginkgo.By("Run a high priority pod that use 60% of a node resources.")
		// Create a high priority pod and make sure it is scheduled.
		runPausePod(f, pausePodConfig{
			Name:              "preemptor-pod",
			PriorityClassName: highPriorityClassName,
			Resources: &corev1.ResourceRequirements{
				Requests: podRes,
			},
		})
		// Make sure that the lowest priority pod is deleted.
		preemptedPod, err := cs.CoreV1().Pods(pods[0].Namespace).Get(pods[0].Name, metav1.GetOptions{})
		podDeleted := (err != nil && errors.IsNotFound(err)) ||
			(err == nil && preemptedPod.DeletionTimestamp != nil)
		gomega.Expect(podDeleted).To(gomega.BeTrue())
		// Other pods (mid priority ones) should be present.
		for i := 1; i < len(pods); i++ {
			livePod, err := cs.CoreV1().Pods(pods[i].Namespace).Get(pods[i].Name, metav1.GetOptions{})
			framework.ExpectNoError(err)
			gomega.Expect(livePod.DeletionTimestamp).To(gomega.BeNil())
		}
	})

	// This test verifies that when a critical pod is created and no node with
	// enough resources is found, scheduler preempts a lower priority pod to schedule
	// this critical pod.
	ginkgo.It("validates lower priority pod preemption by critical pod", func() {
		var podRes corev1.ResourceList
		// Create one pod per node that uses a lot of the node's resources.
		ginkgo.By("Create pods that use 60% of node resources.")
		pods := make([]*corev1.Pod, len(nodeList.Items))
		for i, node := range nodeList.Items {
			cpuAllocatable, found := node.Status.Allocatable["cpu"]
			gomega.Expect(found).To(gomega.Equal(true))
			milliCPU := cpuAllocatable.MilliValue() * 40 / 100
			memAllocatable, found := node.Status.Allocatable["memory"]
			gomega.Expect(found).To(gomega.Equal(true))
			memory := memAllocatable.Value() * 60 / 100
			podRes = corev1.ResourceList{}
			podRes[corev1.ResourceCPU] = *resource.NewMilliQuantity(int64(milliCPU), resource.DecimalSI)
			podRes[corev1.ResourceMemory] = *resource.NewQuantity(int64(memory), resource.BinarySI)

			// make the first pod low priority and the rest medium priority.
			priorityName := mediumPriorityClassName
			if i == 0 {
				priorityName = lowPriorityClassName
			}
			pods[i] = createPausePod(f, pausePodConfig{
				Name:              fmt.Sprintf("pod%d-%v", i, priorityName),
				PriorityClassName: priorityName,
				Resources: &corev1.ResourceRequirements{
					Requests: podRes,
				},
			})
			e2elog.Logf("Created pod: %v", pods[i].Name)
		}
		ginkgo.By("Wait for pods to be scheduled.")
		for _, pod := range pods {
			framework.ExpectNoError(e2epod.WaitForPodRunningInNamespace(cs, pod))
		}

		ginkgo.By("Run a critical pod that use 60% of a node resources.")
		// Create a critical pod and make sure it is scheduled.
		runPausePod(f, pausePodConfig{
			Name:              "critical-pod",
			Namespace:         metav1.NamespaceSystem,
			PriorityClassName: scheduling.SystemClusterCritical,
			Resources: &corev1.ResourceRequirements{
				Requests: podRes,
			},
		})
		// Make sure that the lowest priority pod is deleted.
		preemptedPod, err := cs.CoreV1().Pods(pods[0].Namespace).Get(pods[0].Name, metav1.GetOptions{})
		defer func() {
			// Clean-up the critical pod
			err := f.ClientSet.CoreV1().Pods(metav1.NamespaceSystem).Delete("critical-pod", metav1.NewDeleteOptions(0))
			framework.ExpectNoError(err)
		}()
		podDeleted := (err != nil && errors.IsNotFound(err)) ||
			(err == nil && preemptedPod.DeletionTimestamp != nil)
		gomega.Expect(podDeleted).To(gomega.BeTrue())
		// Other pods (mid priority ones) should be present.
		for i := 1; i < len(pods); i++ {
			livePod, err := cs.CoreV1().Pods(pods[i].Namespace).Get(pods[i].Name, metav1.GetOptions{})
			framework.ExpectNoError(err)
			gomega.Expect(livePod.DeletionTimestamp).To(gomega.BeNil())
		}
	})

	// This test verifies that when a high priority pod is pending and its
	// scheduling violates a medium priority pod anti-affinity, the medium priority
	// pod is preempted to allow the higher priority pod schedule.
	// It also verifies that existing low priority pods are not preempted as their
	// preemption wouldn't help.
	ginkgo.It("validates pod anti-affinity works in preemption", func() {
		var podRes corev1.ResourceList
		// Create a few pods that uses a small amount of resources.
		ginkgo.By("Create pods that use 10% of node resources.")
		numPods := 4
		if len(nodeList.Items) < numPods {
			numPods = len(nodeList.Items)
		}
		pods := make([]*corev1.Pod, numPods)
		for i := 0; i < numPods; i++ {
			node := nodeList.Items[i]
			cpuAllocatable, found := node.Status.Allocatable["cpu"]
			gomega.Expect(found).To(gomega.BeTrue())
			milliCPU := cpuAllocatable.MilliValue() * 10 / 100
			memAllocatable, found := node.Status.Allocatable["memory"]
			gomega.Expect(found).To(gomega.BeTrue())
			memory := memAllocatable.Value() * 10 / 100
			podRes = corev1.ResourceList{}
			podRes[corev1.ResourceCPU] = *resource.NewMilliQuantity(int64(milliCPU), resource.DecimalSI)
			podRes[corev1.ResourceMemory] = *resource.NewQuantity(int64(memory), resource.BinarySI)

			// Apply node label to each node
			framework.AddOrUpdateLabelOnNode(cs, node.Name, "node", node.Name)
			framework.ExpectNodeHasLabel(cs, node.Name, "node", node.Name)

			// make the first pod medium priority and the rest low priority.
			priorityName := lowPriorityClassName
			if i == 0 {
				priorityName = mediumPriorityClassName
			}
			pods[i] = createPausePod(f, pausePodConfig{
				Name:              fmt.Sprintf("pod%d-%v", i, priorityName),
				PriorityClassName: priorityName,
				Resources: &corev1.ResourceRequirements{
					Requests: podRes,
				},
				Affinity: &corev1.Affinity{
					PodAntiAffinity: &corev1.PodAntiAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
							{
								LabelSelector: &metav1.LabelSelector{
									MatchExpressions: []metav1.LabelSelectorRequirement{
										{
											Key:      "service",
											Operator: metav1.LabelSelectorOpIn,
											Values:   []string{"blah", "foo"},
										},
									},
								},
								TopologyKey: "node",
							},
						},
					},
					NodeAffinity: &corev1.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
							NodeSelectorTerms: []corev1.NodeSelectorTerm{
								{
									MatchExpressions: []corev1.NodeSelectorRequirement{
										{
											Key:      "node",
											Operator: corev1.NodeSelectorOpIn,
											Values:   []string{node.Name},
										},
									},
								},
							},
						},
					},
				},
			})
			e2elog.Logf("Created pod: %v", pods[i].Name)
		}
		defer func() { // Remove added labels
			for i := 0; i < numPods; i++ {
				framework.RemoveLabelOffNode(cs, nodeList.Items[i].Name, "node")
			}
		}()

		ginkgo.By("Wait for pods to be scheduled.")
		for _, pod := range pods {
			framework.ExpectNoError(e2epod.WaitForPodRunningInNamespace(cs, pod))
		}

		ginkgo.By("Run a high priority pod with node affinity to the first node.")
		// Create a high priority pod and make sure it is scheduled.
		runPausePod(f, pausePodConfig{
			Name:              "preemptor-pod",
			PriorityClassName: highPriorityClassName,
			Labels:            map[string]string{"service": "blah"},
			Affinity: &corev1.Affinity{
				NodeAffinity: &corev1.NodeAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
						NodeSelectorTerms: []corev1.NodeSelectorTerm{
							{
								MatchExpressions: []corev1.NodeSelectorRequirement{
									{
										Key:      "node",
										Operator: corev1.NodeSelectorOpIn,
										Values:   []string{nodeList.Items[0].Name},
									},
								},
							},
						},
					},
				},
			},
		})
		// Make sure that the medium priority pod on the first node is preempted.
		preemptedPod, err := cs.CoreV1().Pods(pods[0].Namespace).Get(pods[0].Name, metav1.GetOptions{})
		podDeleted := (err != nil && errors.IsNotFound(err)) ||
			(err == nil && preemptedPod.DeletionTimestamp != nil)
		gomega.Expect(podDeleted).To(gomega.BeTrue())
		// Other pods (low priority ones) should be present.
		for i := 1; i < len(pods); i++ {
			livePod, err := cs.CoreV1().Pods(pods[i].Namespace).Get(pods[i].Name, metav1.GetOptions{})
			framework.ExpectNoError(err)
			gomega.Expect(livePod.DeletionTimestamp).To(gomega.BeNil())
		}
	})
})

var _ = SIGDescribe("PodPriorityResolution [Serial]", func() {
	var cs clientset.Interface
	var ns string
	f := framework.NewDefaultFramework("sched-pod-priority")

	ginkgo.BeforeEach(func() {
		cs = f.ClientSet
		ns = f.Namespace.Name

		err := framework.CheckTestingNSDeletedExcept(cs, ns)
		framework.ExpectNoError(err)
	})

	// This test verifies that system critical priorities are created automatically and resolved properly.
	ginkgo.It("validates critical system priorities are created and resolved", func() {
		// Create pods that use system critical priorities and
		ginkgo.By("Create pods that use critical system priorities.")
		systemPriorityClasses := []string{
			scheduling.SystemNodeCritical, scheduling.SystemClusterCritical,
		}
		for i, spc := range systemPriorityClasses {
			pod := createPausePod(f, pausePodConfig{
				Name:              fmt.Sprintf("pod%d-%v", i, spc),
				Namespace:         metav1.NamespaceSystem,
				PriorityClassName: spc,
			})
			defer func() {
				// Clean-up the pod.
				err := f.ClientSet.CoreV1().Pods(pod.Namespace).Delete(pod.Name, metav1.NewDeleteOptions(0))
				framework.ExpectNoError(err)
			}()
			gomega.Expect(pod.Spec.Priority).NotTo(gomega.BeNil())
			e2elog.Logf("Created pod: %v", pod.Name)
		}
	})
})

// construct a fakecpu so as to set it to status of Node object
// otherwise if we update CPU/Memory/etc, those values will be corrected back by kubelet
var fakecpu corev1.ResourceName = "example.com/fakecpu"

var _ = SIGDescribe("PreemptionExecutionPath", func() {
	var cs clientset.Interface
	var node *corev1.Node
	var ns, nodeHostNameLabel string
	f := framework.NewDefaultFramework("sched-preemption-path")

	priorityPairs := make([]priorityPair, 0)

	ginkgo.AfterEach(func() {
		// print out additional info if tests failed
		if ginkgo.CurrentGinkgoTestDescription().Failed {
			// list existing priorities
			priorityList, err := cs.SchedulingV1().PriorityClasses().List(metav1.ListOptions{})
			if err != nil {
				e2elog.Logf("Unable to list priorities: %v", err)
			} else {
				e2elog.Logf("List existing priorities:")
				for _, p := range priorityList.Items {
					e2elog.Logf("%v/%v created at %v", p.Name, p.Value, p.CreationTimestamp)
				}
			}
		}

		if node != nil {
			nodeCopy := node.DeepCopy()
			// force it to update
			nodeCopy.ResourceVersion = "0"
			delete(nodeCopy.Status.Capacity, fakecpu)
			_, err := cs.CoreV1().Nodes().UpdateStatus(nodeCopy)
			framework.ExpectNoError(err)
		}
		for _, pair := range priorityPairs {
			cs.SchedulingV1().PriorityClasses().Delete(pair.name, metav1.NewDeleteOptions(0))
		}
	})

	ginkgo.BeforeEach(func() {
		cs = f.ClientSet
		ns = f.Namespace.Name

		// find an available node
		ginkgo.By("Finding an available node")
		nodeName := GetNodeThatCanRunPod(f)
		e2elog.Logf("found a healthy node: %s", nodeName)

		// get the node API object
		var err error
		node, err = cs.CoreV1().Nodes().Get(nodeName, metav1.GetOptions{})
		if err != nil {
			framework.Failf("error getting node %q: %v", nodeName, err)
		}
		var ok bool
		nodeHostNameLabel, ok = node.GetObjectMeta().GetLabels()["kubernetes.io/hostname"]
		if !ok {
			framework.Failf("error getting kubernetes.io/hostname label on node %s", nodeName)
		}

		// update Node API object with a fake resource
		nodeCopy := node.DeepCopy()
		// force it to update
		nodeCopy.ResourceVersion = "0"
		nodeCopy.Status.Capacity[fakecpu] = resource.MustParse("800")
		node, err = cs.CoreV1().Nodes().UpdateStatus(nodeCopy)
		framework.ExpectNoError(err)

		// create four PriorityClass: p1, p2, p3, p4
		for i := 1; i <= 4; i++ {
			priorityName := fmt.Sprintf("p%d", i)
			priorityVal := int32(i)
			priorityPairs = append(priorityPairs, priorityPair{name: priorityName, value: priorityVal})
			_, err := cs.SchedulingV1().PriorityClasses().Create(&schedulerapi.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: priorityName}, Value: priorityVal})
			if err != nil {
				e2elog.Logf("Failed to create priority '%v/%v': %v", priorityName, priorityVal, err)
				e2elog.Logf("Reason: %v. Msg: %v", errors.ReasonForError(err), err)
			}
			gomega.Expect(err == nil || errors.IsAlreadyExists(err)).To(gomega.Equal(true))
		}
	})

	ginkgo.It("runs ReplicaSets to verify preemption running path", func() {
		podNamesSeen := make(map[string]struct{})
		stopCh := make(chan struct{})

		// create a pod controller to list/watch pod events from the test framework namespace
		_, podController := cache.NewInformer(
			&cache.ListWatch{
				ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
					obj, err := f.ClientSet.CoreV1().Pods(ns).List(options)
					return runtime.Object(obj), err
				},
				WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
					return f.ClientSet.CoreV1().Pods(ns).Watch(options)
				},
			},
			&corev1.Pod{},
			0,
			cache.ResourceEventHandlerFuncs{
				AddFunc: func(obj interface{}) {
					if pod, ok := obj.(*corev1.Pod); ok {
						podNamesSeen[pod.Name] = struct{}{}
					}
				},
			},
		)
		go podController.Run(stopCh)
		defer close(stopCh)

		// prepare four ReplicaSet
		rsConfs := []pauseRSConfig{
			{
				Replicas: int32(5),
				PodConfig: pausePodConfig{
					Name:              "pod1",
					Namespace:         ns,
					Labels:            map[string]string{"name": "pod1"},
					PriorityClassName: "p1",
					NodeSelector:      map[string]string{"kubernetes.io/hostname": nodeHostNameLabel},
					Resources: &corev1.ResourceRequirements{
						Requests: corev1.ResourceList{fakecpu: resource.MustParse("40")},
						Limits:   corev1.ResourceList{fakecpu: resource.MustParse("40")},
					},
				},
			},
			{
				Replicas: int32(4),
				PodConfig: pausePodConfig{
					Name:              "pod2",
					Namespace:         ns,
					Labels:            map[string]string{"name": "pod2"},
					PriorityClassName: "p2",
					NodeSelector:      map[string]string{"kubernetes.io/hostname": nodeHostNameLabel},
					Resources: &corev1.ResourceRequirements{
						Requests: corev1.ResourceList{fakecpu: resource.MustParse("50")},
						Limits:   corev1.ResourceList{fakecpu: resource.MustParse("50")},
					},
				},
			},
			{
				Replicas: int32(4),
				PodConfig: pausePodConfig{
					Name:              "pod3",
					Namespace:         ns,
					Labels:            map[string]string{"name": "pod3"},
					PriorityClassName: "p3",
					NodeSelector:      map[string]string{"kubernetes.io/hostname": nodeHostNameLabel},
					Resources: &corev1.ResourceRequirements{
						Requests: corev1.ResourceList{fakecpu: resource.MustParse("95")},
						Limits:   corev1.ResourceList{fakecpu: resource.MustParse("95")},
					},
				},
			},
			{
				Replicas: int32(1),
				PodConfig: pausePodConfig{
					Name:              "pod4",
					Namespace:         ns,
					Labels:            map[string]string{"name": "pod4"},
					PriorityClassName: "p4",
					NodeSelector:      map[string]string{"kubernetes.io/hostname": nodeHostNameLabel},
					Resources: &corev1.ResourceRequirements{
						Requests: corev1.ResourceList{fakecpu: resource.MustParse("400")},
						Limits:   corev1.ResourceList{fakecpu: resource.MustParse("400")},
					},
				},
			},
		}
		// create ReplicaSet{1,2,3} so as to occupy 780/800 fake resource
		rsNum := len(rsConfs)
		for i := 0; i < rsNum-1; i++ {
			runPauseRS(f, rsConfs[i])
		}

		e2elog.Logf("pods created so far: %v", podNamesSeen)
		e2elog.Logf("length of pods created so far: %v", len(podNamesSeen))

		// create ReplicaSet4
		// if runPauseRS failed, it means ReplicaSet4 cannot be scheduled even after 1 minute
		// which is unacceptable
		runPauseRS(f, rsConfs[rsNum-1])

		e2elog.Logf("pods created so far: %v", podNamesSeen)
		e2elog.Logf("length of pods created so far: %v", len(podNamesSeen))

		// count pods number of ReplicaSet{1,2,3}, if it's more than expected replicas
		// then it denotes its pods have been over-preempted
		// "*2" means pods of ReplicaSet{1,2} are expected to be only preempted once
		maxRSPodsSeen := []int{5 * 2, 4 * 2, 4}
		rsPodsSeen := []int{0, 0, 0}
		for podName := range podNamesSeen {
			if strings.HasPrefix(podName, "rs-pod1") {
				rsPodsSeen[0]++
			} else if strings.HasPrefix(podName, "rs-pod2") {
				rsPodsSeen[1]++
			} else if strings.HasPrefix(podName, "rs-pod3") {
				rsPodsSeen[2]++
			}
		}
		for i, got := range rsPodsSeen {
			expected := maxRSPodsSeen[i]
			if got > expected {
				framework.Failf("pods of ReplicaSet%d have been over-preempted: expect %v pod names, but got %d", i+1, expected, got)
			}
		}
	})
})

type pauseRSConfig struct {
	Replicas  int32
	PodConfig pausePodConfig
}

func initPauseRS(f *framework.Framework, conf pauseRSConfig) *appsv1.ReplicaSet {
	pausePod := initPausePod(f, conf.PodConfig)
	pauseRS := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "rs-" + pausePod.Name,
			Namespace: pausePod.Namespace,
		},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: &conf.Replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: pausePod.Labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: pausePod.ObjectMeta.Labels},
				Spec:       pausePod.Spec,
			},
		},
	}
	return pauseRS
}

func createPauseRS(f *framework.Framework, conf pauseRSConfig) *appsv1.ReplicaSet {
	namespace := conf.PodConfig.Namespace
	if len(namespace) == 0 {
		namespace = f.Namespace.Name
	}
	rs, err := f.ClientSet.AppsV1().ReplicaSets(namespace).Create(initPauseRS(f, conf))
	framework.ExpectNoError(err)
	return rs
}

func runPauseRS(f *framework.Framework, conf pauseRSConfig) *appsv1.ReplicaSet {
	rs := createPauseRS(f, conf)
	framework.ExpectNoError(replicaset.WaitForReplicaSetTargetAvailableReplicas(f.ClientSet, rs, conf.Replicas))
	return rs
}
