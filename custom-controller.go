package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	// Read environment variable for inCluster
	inCluster := os.Getenv("inCluster") == "true"

	// Configure Kubernetes client
	var config *rest.Config
	var err error

	if inCluster {
		fmt.Println("Using in-cluster configuration.")
		config, err = rest.InClusterConfig()
		if err != nil {
			panic(fmt.Sprintf("Failed to load in-cluster config: %v", err))
		}
	} else {
		fmt.Println("Using out-of-cluster configuration.")
		// Define a kubeconfig flag
		kubeconfig := flag.String("kubeconfig", "~/.kube/config", "Path to kubeconfig file")
		flag.Parse()

		config, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
		if err != nil {
			panic(fmt.Sprintf("Failed to load kubeconfig: %v", err))
		}
	}

	// Create the Kubernetes client
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	// Start watching pods
	fmt.Println("Starting Pod Reaper controller...")
	err = wait.PollImmediate(10*time.Second, time.Hour, func() (bool, error) {
		err := remediatePods(clientset)
		if err != nil {
			fmt.Printf("Error during remediation: %v\n", err)
			// Log error but continue polling
			return false, nil
		}
		// Always return false to keep polling indefinitely
		return false, nil
	})
	if err != nil {
		panic(err.Error())
	}
}

func remediatePods(clientset *kubernetes.Clientset) error {
	podList, err := clientset.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return err
	}

	for _, pod := range podList.Items {
		if shouldRemediate(pod) {
			fmt.Printf("Remediating pod %s in namespace %s...\n", pod.Name, pod.Namespace)
			err := clientset.CoreV1().Pods(pod.Namespace).Delete(context.TODO(), pod.Name, metav1.DeleteOptions{})
			if err != nil {
				fmt.Printf("Failed to delete pod %s: %v\n", pod.Name, err)
			} else {
				fmt.Printf("Pod %s deleted successfully.\n", pod.Name)
			}
		}
	}

	return nil
}

func shouldRemediate(pod v1.Pod) bool {
	// Check if the pod is in an error state
	for _, containerStatus := range pod.Status.ContainerStatuses {
		if containerStatus.State.Waiting != nil {
			reason := containerStatus.State.Waiting.Reason
			if reason == "CreateContainerError" || reason == "CrashLoopBackOff" {
				return true
			}
		}
	}
	return false
}
