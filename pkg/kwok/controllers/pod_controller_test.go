/*
Copyright 2022 The Kubernetes Authors.

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

package controllers

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/fake"

	"sigs.k8s.io/kwok/pkg/kwok/controllers/templates"
	"sigs.k8s.io/kwok/pkg/log"
)

func TestPodController(t *testing.T) {
	clientset := fake.NewSimpleClientset(
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "pod0",
				Namespace:         "default",
				CreationTimestamp: metav1.Now(),
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "test-container",
						Image: "test-image",
					},
				},
				NodeName: "node0",
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "xxxx",
				Namespace:         "default",
				CreationTimestamp: metav1.Now(),
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "test-container",
						Image: "test-image",
					},
				},
				NodeName: "xxxx",
			},
		},
	)

	nodeHasFunc := func(nodeName string) bool {
		return strings.HasPrefix(nodeName, "node")
	}
	annotationSelector, _ := labels.Parse("fake=custom")
	pods, err := NewPodController(PodControllerConfig{
		ClientSet:                             clientset,
		NodeIP:                                "10.0.0.1",
		CIDR:                                  "10.0.0.1/24",
		DisregardStatusWithAnnotationSelector: annotationSelector.String(),
		PodStatusTemplate:                     templates.DefaultPodStatusTemplate,
		NodeHasFunc:                           nodeHasFunc,
		FuncMap:                               funcMap,
		LockPodParallelism:                    2,
		DeletePodParallelism:                  2,
	})
	if err != nil {
		t.Fatal(fmt.Errorf("new pods controller error: %w", err))
	}

	ctx := context.Background()
	ctx = log.NewContext(ctx, log.NewLogger(os.Stderr, log.DebugLevel))
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	t.Cleanup(func() {
		cancel()
		time.Sleep(time.Second)
	})

	err = pods.Start(ctx)
	if err != nil {
		t.Fatal(fmt.Errorf("start pods controller error: %w", err))
	}

	_, err = clientset.CoreV1().Pods("default").Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "pod1",
			Namespace:         "default",
			CreationTimestamp: metav1.Now(),
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "test-container",
					Image: "test-image",
				},
			},
			NodeName: "node0",
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(fmt.Errorf("create pod1 error: %w", err))
	}

	pod1, err := clientset.CoreV1().Pods("default").Get(ctx, "pod1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(fmt.Errorf("get pod1 error: %w", err))
	}
	pod1.Annotations = map[string]string{
		"fake": "custom",
	}
	pod1.Status.Reason = "custom"
	_, err = clientset.CoreV1().Pods("default").Update(ctx, pod1, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(fmt.Errorf("update pod1 error: %w", err))
	}

	pod1, err = clientset.CoreV1().Pods("default").Get(ctx, "pod1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(fmt.Errorf("get pod1 error: %w", err))
	}
	if pod1.Status.Reason != "custom" {
		t.Fatal(fmt.Errorf("pod1 status reason not custom"))
	}

	var list *corev1.PodList
	err = wait.PollUntilWithContext(ctx, time.Second, func(ctx context.Context) (done bool, err error) {
		list, err = clientset.CoreV1().Pods("default").List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, fmt.Errorf("list pods error: %w", err)
		}
		if len(list.Items) != 3 {
			return false, fmt.Errorf("want 3 pods, got %d", len(list.Items))
		}
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	pod := list.Items[0]
	now := metav1.Now()
	pod.DeletionTimestamp = &now
	_, err = clientset.CoreV1().Pods("default").Update(ctx, &pod, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(fmt.Errorf("delete pod error: %w", err))
	}

	err = wait.PollUntilWithContext(ctx, time.Second, func(ctx context.Context) (done bool, err error) {
		list, err = clientset.CoreV1().Pods("default").List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, fmt.Errorf("list pods error: %w", err)
		}
		if len(list.Items) != 2 {
			return false, fmt.Errorf("want 2 pods, got %d", len(list.Items))
		}
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, pod := range list.Items {
		if nodeHasFunc(pod.Spec.NodeName) {
			if pod.Status.Phase != corev1.PodRunning {
				t.Fatal(fmt.Errorf("want pod %s phase is running, got %s", pod.Name, pod.Status.Phase))
			}
		} else {
			if pod.Status.Phase == corev1.PodRunning {
				t.Fatal(fmt.Errorf("want pod %s phase is not running, got %s", pod.Name, pod.Status.Phase))
			}
		}
	}
}
