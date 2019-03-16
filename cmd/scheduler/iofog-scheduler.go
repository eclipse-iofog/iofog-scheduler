/*
 *  *******************************************************************************
 *  * Copyright (c) 2019 Edgeworx, Inc.
 *  *
 *  * This program and the accompanying materials are made available under the
 *  * terms of the Eclipse Public License v. 2.0 which is available at
 *  * http://www.eclipse.org/legal/epl-2.0
 *  *
 *  * SPDX-License-Identifier: EPL-2.0
 *  *******************************************************************************
 *
 */

package cmd

import (
	"context"
	"fmt"
	"k8s.io/client-go/rest"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"errors"
	"github.com/spf13/cobra"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	listersv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

const schedulerName = "iofog-custom-scheduler"

type predicateFunc func(node *v1.Node, pod *v1.Pod) bool
type priorityFunc func(node *v1.Node, pod *v1.Pod) int

type Scheduler struct {
	clientset  *kubernetes.Clientset
	podQueue   chan *v1.Pod
	nodeLister listersv1.NodeLister
	predicates []predicateFunc
	priorities []priorityFunc
}

var kubeConfig string
var rootContext, rootContextCancel = context.WithCancel(context.Background())

var RootCmd = &cobra.Command{
	Use:   "iofog-scheduler",
	Short: "iofog-scheduler is a kubernetes custom scheduler for ioFog ECN.",
	Run: func(cmd *cobra.Command, args []string) {
		defer rootContextCancel()

		execute()

		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sig
			rootContextCancel()
		}()
		select {}
	},
}

func newScheduler(podQueue chan *v1.Pod) Scheduler {
	var config *rest.Config

	// Check if the kubeConfig file exists.
	if _, err := os.Stat(kubeConfig); !os.IsNotExist(err) {
		// Get the kubernetes config from the filepath.
		config, err = clientcmd.BuildConfigFromFlags("", kubeConfig)
		if err != nil {
			log.Fatal(err)
		}
	} else {
		// Set to in-cluster config.
		config, err = rest.InClusterConfig()
		if err != nil {
			log.Fatal(err)
		}
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatal(err)
	}

	return Scheduler{
		clientset:  clientset,
		podQueue:   podQueue,
		nodeLister: initInformers(clientset, podQueue),
		predicates: []predicateFunc{
			nodePredicate,
		},
		priorities: []priorityFunc{
			lessPodsPriority,
		},
	}
}

func initInformers(clientset *kubernetes.Clientset, podQueue chan *v1.Pod) listersv1.NodeLister {
	factory := informers.NewSharedInformerFactory(clientset, 0)

	nodeInformer := factory.Core().V1().Nodes()
	nodeInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			node, ok := obj.(*v1.Node)
			if !ok {
				return
			}
			log.Printf("new node added: %s", node.GetName())
		},
	})

	podInformer := factory.Core().V1().Pods()
	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pod, ok := obj.(*v1.Pod)
			if !ok {
				return
			}
			if pod.Spec.NodeName == "" && pod.Spec.SchedulerName == schedulerName {
				podQueue <- pod
			}
		},
	})

	factory.Start(rootContext.Done())
	return nodeInformer.Lister()
}

func execute() {
	fmt.Println("starting iofog-scheduler!")

	podQueue := make(chan *v1.Pod, 300)
	defer close(podQueue)

	scheduler := newScheduler(podQueue)
	scheduler.Run()
}

func lessPodsPriority(node *v1.Node, pod *v1.Pod) int {
	return int(node.Status.Allocatable.Pods().Value())
}

func init() {
	RootCmd.PersistentFlags().StringVar(&kubeConfig, "kubeconfig", "", "config file (default is $HOME/.kube/config)")
}

func nodePredicate(node *v1.Node, pod *v1.Pod) bool {
	if !strings.HasPrefix(node.Name, "iofog-") {
		return false
	}

	if node.Status.Allocatable.Pods().Value() <= 0 {
		return false
	}

	for _, status := range node.Status.Conditions {
		if status.Status == "True" && status.Type == "Ready" {
			return true
		}
	}

	return false
}

func (s *Scheduler) Run() {
	wait.Until(s.ScheduleOne, 0, rootContext.Done())
}

func (s *Scheduler) ScheduleOne() {

	p := <-s.podQueue
	fmt.Println("found a pod to schedule:", p.Namespace, "/", p.Name)

	node, err := s.findFit(p)
	if err != nil {
		log.Println("cannot find node that fits pod", err.Error())
		return
	}

	err = s.bindPod(p, node)
	if err != nil {
		log.Println("failed to bind pod", err.Error())
		return
	}

	message := fmt.Sprintf("placed pod [%s/%s] on %s\n", p.Namespace, p.Name, node)

	err = s.emitEvent(p, message)
	if err != nil {
		log.Println("failed to emit scheduled event", err.Error())
		return
	}

	fmt.Println(message)
}

func (s *Scheduler) findFit(pod *v1.Pod) (string, error) {
	nodes, err := s.nodeLister.List(labels.Everything())
	if err != nil {
		return "", err
	}

	filteredNodes := s.runPredicates(nodes, pod)
	if len(filteredNodes) == 0 {
		return "", errors.New("failed to find node that fits pod")
	}
	priorities := s.prioritize(filteredNodes, pod)
	return s.findBestNode(priorities), nil
}

func (s *Scheduler) bindPod(p *v1.Pod, node string) error {
	return s.clientset.CoreV1().Pods(p.Namespace).Bind(&v1.Binding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.Name,
			Namespace: p.Namespace,
		},
		Target: v1.ObjectReference{
			APIVersion: "v1",
			Kind:       "Node",
			Name:       node,
		},
	})
}

func (s *Scheduler) emitEvent(p *v1.Pod, message string) error {
	timestamp := time.Now().UTC()
	_, err := s.clientset.CoreV1().Events(p.Namespace).Create(&v1.Event{
		Count:          1,
		Message:        message,
		Reason:         "Scheduled",
		LastTimestamp:  metav1.NewTime(timestamp),
		FirstTimestamp: metav1.NewTime(timestamp),
		Type:           "Normal",
		Source: v1.EventSource{
			Component: schedulerName,
		},
		InvolvedObject: v1.ObjectReference{
			Kind:      "Pod",
			Name:      p.Name,
			Namespace: p.Namespace,
			UID:       p.UID,
		},
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: p.Name + "-",
		},
	})
	if err != nil {
		return err
	}

	return nil
}

func (s *Scheduler) runPredicates(nodes []*v1.Node, pod *v1.Pod) []*v1.Node {
	filteredNodes := make([]*v1.Node, 0)
	for _, node := range nodes {
		if s.predicatesApply(node, pod) {
			filteredNodes = append(filteredNodes, node)
		}
	}

	return filteredNodes
}

func (s *Scheduler) predicatesApply(node *v1.Node, pod *v1.Pod) bool {
	for _, predicate := range s.predicates {
		if !predicate(node, pod) {
			return false
		}
	}

	return true
}

func (s *Scheduler) prioritize(nodes []*v1.Node, pod *v1.Pod) map[string]int {
	priorities := make(map[string]int)
	for _, node := range nodes {
		for _, priority := range s.priorities {
			priorities[node.Name] += priority(node, pod)
		}
	}

	return priorities
}

func (s *Scheduler) findBestNode(priorities map[string]int) string {
	var max int
	var bestNode string
	for node, p := range priorities {
		if p > max {
			max = p
			bestNode = node
		}
	}
	return bestNode
}
