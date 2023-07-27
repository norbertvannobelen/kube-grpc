package kubegrpc

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
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
	grpcConnection []*GrpcConnection
}

// connHealth - Used to decouple events to reduce locking
type connHealth struct {
	functions GrpcKubeBalancer
	grpcConn  *GrpcConnection
}

// connUpdate - Used to decouple events to reduce locking
type connUpdate struct {
	serviceName string
	conn        *connection
}

// GrpcConnction - Externally accessible grpc connection data for in pool array (from connection.grpcConnection)
type GrpcConnection struct {
	GrpcConnection interface{}
	connectionIP   string
	serviceName    string
	conn           *grpc.ClientConn
}

var (
	clientset        *kubernetes.Clientset
	connectionCache  = make(map[string]*connection) // contains all managed connections
	mutex            = &sync.RWMutex{}
	dirtyConnections = make(chan *GrpcConnection)
)

func init() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("(kube-grpc) FATAL: init(): Could not get kube config in cluster. Error: %v", err)
	}
	clientset, err = kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("(kube-grpc) FATAL: init(): Could not connect to kube cluster with config. Error: %v", err)
	}
	poolManager()
}

// poolManager - Updates the existing connection pools, keeps the pools healthy
// Runs once per second in which it pings existing connections.
// If a connection has failed, the connection is removed from the pool and a scan is executed for new connections.
// Every 60 seconds a full scan is done to check for new pods which might have been scaled into the pool
func poolManager() {
	go cleanConnections()
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
		a := make([]*connHealth, 0)
		mutex.RLock()
		for _, v := range connectionCache {
			// Iterate over the connections while calling the provided ping function
			for _, c := range v.grpcConnection {
				// Decouple mutex lock from actual ping to reduce lock time by using intermediate array for the pointers
				a = append(a, &connHealth{functions: v.functions, grpcConn: c})
			}
		}
		mutex.RUnlock()
		// Iterate over array of connection pointers
		for _, v := range a {
			go func(grpcConn *GrpcConnection, f GrpcKubeBalancer) {
				err := f.Ping(grpcConn.GrpcConnection)
				if err != nil {
					// Add to dirtyConnections channel:
					log.Printf("(kube-grpc) INFO: healthcheck(): Failed to ping %s at ip %s",
						grpcConn.serviceName, grpcConn.connectionIP)
					dirtyConnections <- grpcConn
				}
			}(v.grpcConn, v.functions)
		}
	}
}

// cleanConnections - Processes the connections which are stale/can not be reached and removes them from the cache
func cleanConnections() {
	// Not using a channel for this since we want unique services to be updated only (And the map deduplicates the list automatically
	for {
		v := <-dirtyConnections
		mutex.Lock()
		conns := connectionCache[v.serviceName]
		// healthCheck and updatePool could both run this routine at the same time, leading to a change on range conns.grpcConnection
		// and subsequent non-existent just found key. mutex.Lock should protect this code against race conditions.
		for k, gc := range conns.grpcConnection {
			if gc == v {
				go v.conn.Close() // Close open connections just in case there is a non-implementation of the healthcheck or other failure making the connection not terminate
				// Remove connection from slice of connections
				conns.grpcConnection[k] = conns.grpcConnection[len(conns.grpcConnection)-1]
				conns.grpcConnection = conns.grpcConnection[:len(conns.grpcConnection)-1]
				conns.nConnections = len(conns.grpcConnection)
				// Value found, so no need (and very unwanted) to continue iteration since we effectively changed the iterator of the for inner for loop
				break
			}
		}
		log.Printf("(kube-grpc) INFO: cleanConnections(): Pool %s after clean: %v", v.serviceName, conns)
		mutex.Unlock()
	}
}

// updatePool - Every minute a full scan is done to check for new pods which might have been scaled into the pool
func updatePool() {
	for {
		time.Sleep(time.Minute)
		a := make([]*connUpdate, 0)
		mutex.RLock()
		// Make a non-blocking array for update purposes
		for serviceName, v := range connectionCache {
			a = append(a, &connUpdate{serviceName: serviceName, conn: v})
		}
		mutex.RUnlock()
		for _, v := range a {
			updateConnectionPool(v.serviceName, v.conn, true)
		}
	}
}

// Connect - Call to get a connection to the given service and namespace. Will initialize a connection if not yet initialized
// Function wraps Pool function fior backward compatibility. Locking is managed by the pool function
func Connect(serviceName string, f GrpcKubeBalancer) (interface{}, error) {
	_, grcpConn, err := Pool(serviceName, f)
	if err != nil {
		return nil, err
	}
	return grcpConn, nil
}

// Pool - Call to get a connection to the given service and namespace. Will initialize a connection if not yet initialized
// Returns an array of grpcConnections. This array should be locked before any actions are written against it.
// Also returns a singular connection so that the Connect function can use the Pool function without having to implement its own locking
func Pool(serviceName string, f GrpcKubeBalancer) ([]*GrpcConnection, interface{}, error) {
	// Using Lock instead of RLock: Multiple connection requests can come in at high freq.
	// Lock prevents trying to create multiple connections to the same target at once
	mutex.Lock()
	defer mutex.Unlock()
	currentConnection := connectionCache[serviceName]
	if currentConnection == nil {
		currentConnection = &connection{
			nConnections:   0,
			functions:      f,
			grpcConnection: make([]*GrpcConnection, 0),
		}
		connectionCache[serviceName] = currentConnection
	}
	if currentConnection.nConnections == 0 {
		err := initCurrentConnection(serviceName, currentConnection)
		if err != nil {
			return nil, nil, err
		}
	}
	// Not reaching this with 0 connections in the pool (still within the same lock)
	grcpConn := currentConnection.grpcConnection[rand.Intn(currentConnection.nConnections)]
	return currentConnection.grpcConnection, grcpConn.GrpcConnection, nil
}

// ListPool() - Returns the connections currently in the pool
// Possible usages:
// - Implement a secondary way to use connections initialized and managed by kube-grpc
// usage: Unsafe if locking is not implemented correct: The returned value is a reference, not a copy!! Connections might be re-instantiated on crash or for other reasons.
// The developer has to manage failures and might have to call this functions again to get a new/updated connection pool.
func ListPool(serviceName string) []*GrpcConnection {
	mutex.RLock()
	defer mutex.RUnlock()
	currentConnection := connectionCache[serviceName]
	if currentConnection == nil {
		return nil
	}
	return currentConnection.grpcConnection
}

// initCurrentConnection - Tries to update the connection cache on connect.
// If it fails, it will retry for max 3 times to see if the error encountered is transient in nature
func initCurrentConnection(serviceName string, currentConnection *connection) error {
	var err error
	for i := 0; i < 3; i++ {
		err = updateConnectionPool(serviceName, currentConnection, false)
		if err != nil {
			switch err.Error() {
			case "K8S interaction not possible, non-retryable":
				return err
			case "no connections made, retry later":
				// Sleep a second (which is about a lifetime in well configured system)
				time.Sleep(time.Second)
				continue
			}
			return err
		}
	}
	return nil
}

// updateConnectionPool - Sets up the actual connections in the connectionpool
// Also capable of refreshing the pool
// Depending on the access path, a sync.Lock might already be in place, lock (bool) false will skip locking in this function
func updateConnectionPool(serviceName string, currentConnection *connection, lock bool) error {
	// Chat with k8s for service and pod information, slow not blocking action
	svc, namespace, err := getService(serviceName, clientset.CoreV1())
	if err != nil {
		log.Printf("(kube-grpc) ERROR: updateConnectionPool(): Problem updating pool for service %s. Error %v", serviceName, err)
		return errors.New("K8S interaction not possible, non-retryable")
	}
	pods, podErr := getPodsForSvc(svc, namespace, clientset.CoreV1())
	if podErr != nil {
		log.Printf("(kube-grpc) ERROR: updateConnectionPool(): Problem updating pool for service %s. Can not get pods. Error %v",
			serviceName, err)
		return errors.New("K8S interaction not possible, non-retryable")
	}

	log.Printf("(kube-grpc) INFO: updateConnectionPool(): %d pods listed by k8s for service %s", len(pods.Items), serviceName)

	// Evict from pool
	// Disconnect locking reads and eviction channel:
	a := make([]*GrpcConnection, 0)
	// bool lock prevents deadlocks
	if lock {
		// Use a read lock since we do not care if a connection is evicted multiple times (will just do nothing)
		mutex.RLock()
	}
	for _, p := range currentConnection.grpcConnection {
		evict := true
		for _, pod := range pods.Items {
			if p.connectionIP == pod.Status.PodIP {
				log.Printf("(kube-grpc) INFO: updateConnectionPool(): Not evicting %s for %s", p.connectionIP, p.serviceName)
				evict = false
				break
			}
		}
		if evict {
			log.Printf("(kube-grpc) INFO: updateConnectionPool(): Evicting %s for %s", p.connectionIP, p.serviceName)
			// decouple mutex
			a = append(a, p)
		}
	}
	if lock {
		mutex.RUnlock()
	}
	// since channel dirtyConnections locks and a lock might already be in place, let cleanup run from go routine
	// go routine will block until lock is released from either end of this function and fallback to caller,
	// or no lock is in place in which case it might or might not lock until the next lock is called in this function
	go func() {
		for _, p := range a {
			dirtyConnections <- p
		}
	}()

	// Lock only if required and at the last moment to prevent slow k8s query from locking all actions
	if lock {
		mutex.Lock()
		defer mutex.Unlock()
	}
	// Add new connections to pool
	for _, pod := range pods.Items {
		// Check pool for  presense of podIP to prevent duplicate connections:
		ipFound := false
		for _, p := range currentConnection.grpcConnection {
			if p.connectionIP == pod.Status.PodIP {
				ipFound = true
				break
			}
		}
		if ipFound {
			// Ip found, connection alreay present, continue with the next pod:
			continue
		}
		// Pod fully initialized? (k8s connected it to the network?), if not, skip
		if pod.Status.PodIP == "" {
			continue
		}
		portSlice := strings.Split(serviceName, ":")
		if len(portSlice) < 2 {
			log.Fatalf("(kube-grpc) FATAL: updateConnectionPool(): No port number supplied in service as stated in README")
		}
		conn, err := grpc.Dial(pod.Status.PodIP+":"+portSlice[1], grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			// Connection could not be made, so abort, but still try next pods in list
			continue
		}
		grpcConn, err := currentConnection.functions.NewGrpcClient(conn)
		if err != nil {
			// Connection could not be made, so abort, but still try next pods in list
			if conn != nil {
				conn.Close()
			}
			continue
		}
		// add to connection cache
		currentConnection.grpcConnection = append(currentConnection.grpcConnection, &GrpcConnection{
			connectionIP:   pod.Status.PodIP,
			GrpcConnection: grpcConn,
			serviceName:    serviceName, // Added to make use of channel for cleaning up connections easier (compare on key)
			conn:           conn,
		})
		currentConnection.nConnections = len(currentConnection.grpcConnection)
		log.Printf("(kube-grpc) INFO: updateConnectionPool(): Created connection for service %s in namespace %s. Connection pool status %+v",
			serviceName, namespace, currentConnection)
	}
	// Connection pool update might have lead to no connections at all, return appropriate error:
	if currentConnection.nConnections == 0 {
		return errors.New("no connections made, retry later")
	}
	return nil
}

func getService(serviceName string, k8sClient typev1.CoreV1Interface) (*corev1.Service, string, error) {
	listOptions := metav1.ListOptions{}
	serviceSlice := strings.Split(serviceName, ".")
	if len(serviceSlice) < 2 {
		return nil, "", fmt.Errorf("service name not according to convention defined in README. Service name: %s", serviceName)
	}
	namespace := serviceSlice[1]
	svcs, err := k8sClient.Services(namespace).List(context.Background(), listOptions)
	if err != nil {
		return nil, "", fmt.Errorf("cannot retrieve the services list via the k8s API. Error: %v", err)
	}
	svcComponents := strings.Split(serviceName, ".")
	for _, svc := range svcs.Items {
		if svc.Name == svcComponents[0] {
			return &svc, namespace, nil
		}
	}
	return nil, namespace, errors.New("cannot find service")
}

func getPodsForSvc(svc *corev1.Service, namespace string, k8sClient typev1.CoreV1Interface) (*corev1.PodList, error) {
	set := labels.Set(svc.Spec.Selector)
	listOptions := metav1.ListOptions{LabelSelector: set.AsSelector().String()}
	pods, err := k8sClient.Pods(namespace).List(context.Background(), listOptions)
	return pods, err
}
