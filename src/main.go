package kubegrpc

import (
	"errors"
	"log"
	"strings"

	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	typev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

// GrpcKubeBalancer - defines the interface required to setup the actual grpc connection
type GrpcKubeBalancer interface {
	NewGrpcClient(conn *grpc.ClientConn) (*interface{}, error)
}

type connection struct {
	nConnections   int16 // The number of connections
	functions      *GrpcKubeBalancer
	grpcConnection []*grpcConnection
}

type grpcConnection struct {
	grpcConnection *interface{}
	connectionIP   string
}

var (
	clientset       *kubernetes.Clientset
	connectionCache = make(map[string]*connection)
)

func init() {
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("ERROR: init(): Could not get kube config in cluster. Error:" + err.Error())
	}
	clientset, err = kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("ERROR: init(): Could not connect to kube cluster with config. Error:" + err.Error())
	}
}

func main() {
	// List ips from service to connect to:
	// serviceName := "elasticsearch-data"
	// svc, _ := getService(serviceName, "inca", clientset.CoreV1())
	// pods, _ := getPodsForSvc(svc, "inca", clientset.CoreV1())
	// buildConnections(serviceName, pods)
}

// Connect - Call to get a connection to the given service and namespace. Will initialize a connection if not yet initialized
func Connect(serviceName, namespace string, f *GrpcKubeBalancer) (*interface{}, error) {
	currentCache := connectionCache[serviceName]
	if currentCache == nil {
		conn := getConnection(serviceName, namespace, f)
		conn.grpcConnection
	}
	return nil, nil
}

func getConnection(serviceName, namespace string, f *GrpcKubeBalancer) *connection {
	svc, _ := getService(serviceName, namespace, clientset.CoreV1())
	pods, _ := getPodsForSvc(svc, namespace, clientset.CoreV1())
	return buildConnections(serviceName, pods, f)
}

// func grpcServiceConnection(serviceName string) (*grpc.ClientConn) {
// 	return something from map
// }

// buildConnections - Builds up a connection pool, initializes pool when absent
func buildConnections(serviceName string, pods *corev1.PodList, f *GrpcKubeBalancer) *connection {
	currentCache := connectionCache[serviceName]
	if currentCache == nil {
		c := &connection{
			nConnections:   0,
			functions:      f,
			grpcConnection: make([]*grpcConnection, 0),
		}
		connectionCache[serviceName] = c
		currentCache = connectionCache[serviceName]
	}
	for _, pod := range pods.Items {
		conn, err := grpc.Dial(pod.Status.PodIP, grpc.WithInsecure())
		grpcConn, err := (*currentCache.functions).NewGrpcClient(conn)
		if err != nil {
			// Connection could not be made, so abort
			continue
		}
		// add to connection cache
		gc := &grpcConnection{
			connectionIP:   pod.Status.PodIP,
			grpcConnection: grpcConn,
		}
		currentCache.grpcConnection = append(currentCache.grpcConnection, gc)
	}
	return currentCache
}

func getService(serviceName string, namespace string, k8sClient typev1.CoreV1Interface) (*corev1.Service, error) {
	listOptions := metav1.ListOptions{}
	svcs, err := k8sClient.Services(namespace).List(listOptions)
	if err != nil {
		log.Fatal(err)
	}
	for _, svc := range svcs.Items {
		if strings.Contains(svc.Name, serviceName) {
			return &svc, nil
		}
	}
	return nil, errors.New("cannot find service")
}

func getPodsForSvc(svc *corev1.Service, namespace string, k8sClient typev1.CoreV1Interface) (*corev1.PodList, error) {
	set := labels.Set(svc.Spec.Selector)
	listOptions := metav1.ListOptions{LabelSelector: set.AsSelector().String()}
	pods, err := k8sClient.Pods(namespace).List(listOptions)
	return pods, err
}
