package kubegrpc

import (
	"errors"
	"fmt"
	"log"
	"math/rand"
	"strings"
	"sync"
	"time"

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
	NewGrpcClient(conn *grpc.ClientConn) (interface{}, error)
	Ping(grpcConnection interface{}) error
}

type connection struct {
	nConnections   int // The number of connections
	functions      GrpcKubeBalancer
	grpcConnection []*grpcConnection
}

type grpcConnection struct {
	grpcConnection interface{}
	connectionIP   string
	serviceName    string
}

var (
	clientset       *kubernetes.Clientset
	connectionCache = make(map[string]*connection)
	mutex           = &sync.RWMutex{}
)

func init() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("ERROR: init(): Could not get kube config in cluster. Error:" + err.Error())
	}
	clientset, err = kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("ERROR: init(): Could not connect to kube cluster with config. Error:" + err.Error())
	}
	poolManager()
}

// poolManager - Updates the existing connection pools, keeps the pools healthy
// Runs once per second in which it pings existing connections.
// If a connection has failed, the connection is removed from the pool and a scan is executed for new connections.
// Every 60 seconds a full scan is done to check for new pods which might have been scaled into the pool
func poolManager() {
	go healthCheck()
	go updatePool()
}

// healthCheck - Runs once per second in which it pings existing connections.
// If a connection has failed, the connection is removed from the pool and a scan is executed for new connections.
// Currently there is no
func healthCheck() {
	for {
		time.Sleep(time.Second)
		// To prevent conflicts in the loops checking the connections, we use a channel without a listener active
		// The connections are a global variable
		mutex.Lock()
		for _, v := range connectionCache {
			// Iterate over the connections while calling the provided ping function
			dirtyConnections := make(chan *grpcConnection, len(v.grpcConnection))
			for _, c := range v.grpcConnection {
				err := v.functions.Ping(c.grpcConnection)
				if err != nil {
					// Add to dirtyConnections channel:
					log.Printf("INFO: healthcheck(): Pinging %d services with %d pods failed to ping %s at ip %s",
						len(connectionCache), len(v.grpcConnection), c.serviceName, c.connectionIP)
					dirtyConnections <- c
				}
			}
			close(dirtyConnections)
			cleanConnections(dirtyConnections)
		}
		mutex.Unlock()
	}
}

// cleanConnections - Processes the connections which are stale/can not be reached and removes them from the cache
func cleanConnections(dirtyConnections chan *grpcConnection) {
	// Not using a channel for this since we want unique services to be updated only (And the map deduplicates the list automatically
	for v := range dirtyConnections {
		conns := connectionCache[v.serviceName]
		// healthCheck and updatePool could both run this routine at the same time, leading to a change on range conns.grpcConnection
		// and subsequent non-existent just found key. mutex.Lock should protect this code against race conditions.
		for k, gc := range conns.grpcConnection {
			if gc == v {
				// Remove connection from slice of connections
				conns.grpcConnection[k] = conns.grpcConnection[len(conns.grpcConnection)-1]
				conns.grpcConnection = conns.grpcConnection[:len(conns.grpcConnection)-1]
				conns.nConnections = len(conns.grpcConnection)
				// Value found, so no need (and very unwanted) to continue iteration since we effectively changed the iterator of the for inner for loop
				break
			}
		}
		log.Printf("INFO: cleanConnections(): Pool %s after clean: %v", v.serviceName, conns)
	}
}

// updatePool - Every minute a full scan is done to check for new pods which might have been scaled into the pool
func updatePool() {
	for {
		time.Sleep(time.Minute)
		mutex.Lock()
		for serviceName, v := range connectionCache {
			log.Printf("INFO: updatePool(): Updating pool %s", serviceName)
			updateConnectionPool(serviceName, v.functions, v)
		}
		mutex.Unlock()
	}
}

// Connect - Call to get a connection to the given service and namespace. Will initialize a connection if not yet initialized
func Connect(serviceName string, f GrpcKubeBalancer) (interface{}, error) {
	mutex.Lock()
	currentCache := connectionCache[serviceName]
	if currentCache == nil {
		c := &connection{
			nConnections:   0,
			functions:      f,
			grpcConnection: make([]*grpcConnection, 0),
		}
		connectionCache[serviceName] = c
		currentCache = updateConnectionPool(serviceName, f, c)
	}
	grcpConn := currentCache.grpcConnection[rand.Intn(currentCache.nConnections)]
	log.Printf("INFO: Connect(): Chatting with service %s through grpc connection %+v", serviceName, grcpConn)
	mutex.Unlock()
	return grcpConn.grpcConnection, nil
}

func updateConnectionPool(serviceName string, f GrpcKubeBalancer, currentCache *connection) *connection {
	svc, namespace, err := getService(serviceName, clientset.CoreV1())
	if err != nil {
		log.Printf("ERROR: updateConnectionPool(): Problem updating pool for service %s. Error %v", serviceName, err)
		return nil
	}
	pods, podErr := getPodsForSvc(svc, namespace, clientset.CoreV1())
	if podErr != nil {
		log.Printf("ERROR: updateConnectionPool(): Problem updating pool for service %s. Can not get pods. Error %v", serviceName, err)
		return nil
	}

	log.Printf("INFO: updateConnectionPool(): #pods: %d found for service %s", len(pods.Items), serviceName)
	// Evict from pool
	dirtyConnections := make(chan *grpcConnection, len(currentCache.grpcConnection))
	for _, p := range currentCache.grpcConnection {
		evict := true
		for _, pod := range pods.Items {
			if p.connectionIP == pod.Status.PodIP {
				log.Printf("INFO: updateConnectionPool(): Not evicting %s for %s", p.connectionIP, p.serviceName)
				evict = false
				break
			}
		}
		if evict {
			log.Printf("INFO: updateConnectionPool(): Evicting %s for %s", p.connectionIP, p.serviceName)
			dirtyConnections <- p
		}
	}
	close(dirtyConnections)
	cleanConnections(dirtyConnections)
	log.Printf("INFO: updateConnectionPool(): After evict pool for service %s in namespace %s: %+v", serviceName, namespace, currentCache)

	// Add new connections to pool
	for _, pod := range pods.Items {
		// Check pool for  presense of podIP to prevent duplicate connections:
		ipFound := false
		for _, p := range currentCache.grpcConnection {
			if p.connectionIP == pod.Status.PodIP {
				ipFound = true
				break
			}
		}
		if ipFound {
			// Ip found, connection alreay present, continue with the next pod:
			continue
		}
		portSlice := strings.Split(serviceName, ":")
		if len(portSlice) < 2 {
			log.Fatalf("updateConnectionPool(): No port number supplied in service as stated in README")
		}
		conn, err := grpc.Dial(pod.Status.PodIP+":"+portSlice[1], grpc.WithInsecure())
		grpcConn, err := currentCache.functions.NewGrpcClient(conn)
		if err != nil {
			// Connection could not be made, so abort, but still try next pods in list
			continue
		}
		// add to connection cache
		gc := &grpcConnection{
			connectionIP:   pod.Status.PodIP,
			grpcConnection: grpcConn,
			serviceName:    serviceName, // Added to make use of channel for cleaning up connections easier
		}
		currentCache.grpcConnection = append(currentCache.grpcConnection, gc)
		currentCache.nConnections = len(currentCache.grpcConnection)
		connectionCache[serviceName] = currentCache
		log.Printf("INFO: updateConnectionPool(): Created connection for service %s in namespace %s. Connection pool status %+v", serviceName, namespace, currentCache)
	}
	return currentCache
}

func getService(serviceName string, k8sClient typev1.CoreV1Interface) (*corev1.Service, string, error) {
	listOptions := metav1.ListOptions{}
	serviceSlice := strings.Split(serviceName, ".")
	if len(serviceSlice) < 2 {
		return nil, "", fmt.Errorf("Service name not according to convention defined in README. Service name: %s", serviceName)
	}
	namespace := serviceSlice[1]
	svcs, err := k8sClient.Services(namespace).List(listOptions)
	if err != nil {
		log.Fatal(err)
	}
	svcComponents := strings.Split(serviceName, ".")
	for _, svc := range svcs.Items {
		if strings.Contains(svc.Name, svcComponents[0]) {
			return &svc, namespace, nil
		}
	}
	return nil, namespace, errors.New("cannot find service")
}

func getPodsForSvc(svc *corev1.Service, namespace string, k8sClient typev1.CoreV1Interface) (*corev1.PodList, error) {
	set := labels.Set(svc.Spec.Selector)
	listOptions := metav1.ListOptions{LabelSelector: set.AsSelector().String()}
	pods, err := k8sClient.Pods(namespace).List(listOptions)
	return pods, err
}
