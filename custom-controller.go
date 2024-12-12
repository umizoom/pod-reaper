package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
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

	// Configure timezone for timestamps
	timezone := os.Getenv("TIMEZONE")
	if timezone == "" {
		timezone = "UTC"
	}
	loc, timeErr := time.LoadLocation(timezone)

	if timeErr != nil {
		panic(fmt.Sprintf("Invalid timezone: %v", timeErr))
		}

	// Configure Kubernetes client
	var config *rest.Config
	var err error

	if inCluster {
		log("Using in-cluster configuration.", loc)
		config, err = rest.InClusterConfig()
		if err != nil {
			panic(fmt.Sprintf("Failed to load in-cluster config: %v", err))
		}
	} else {
		log("Using out-of-cluster configuration.", loc)
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
  log("Starting Pod Reaper controller...", loc)

  // Create a context that can be canceled
  ctx, cancel := context.WithCancel(context.Background())
  defer cancel()

  // Set up signal handling for graceful shutdown
  sigterm := make(chan os.Signal, 1)
  signal.Notify(sigterm, syscall.SIGTERM, syscall.SIGINT)

  go func() {
	  <-sigterm
	  log("Received SIGTERM, shutting down gracefully...", loc)
	  cancel()
  }()

  wait.UntilWithContext(ctx, func(ctx context.Context) {
	  err = remediatePods(clientset, loc)
	  if err != nil {
		log(fmt.Sprintf("Error during remediation: %v", err), loc)
	  }
  }, 10*time.Second)

  log("Polling stopped due to context cancellation.", loc)
}

func remediatePods(clientset *kubernetes.Clientset, loc *time.Location) error {
	podList, err := clientset.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return err
	}

	for _, pod := range podList.Items {
		if shouldRemediate(pod) {
			log(fmt.Sprintf("Remediating pod %s in namespace %s...", pod.Name, pod.Namespace), loc)
			err := clientset.CoreV1().Pods(pod.Namespace).Delete(context.TODO(), pod.Name, metav1.DeleteOptions{})
			if err != nil {
				log(fmt.Sprintf("Failed to delete pod %s: %v", pod.Name, err), loc)
			} else {
				log(fmt.Sprintf("Pod %s deleted successfully.", pod.Name), loc)
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

func log(message string, loc *time.Location) {
	timestamp := time.Now().In(loc).Format("2006-01-02 15:04:05 MST")
	fmt.Printf("[%s] %s\n", timestamp, message)
}
