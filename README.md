# kube-grpc

Kubernetes grpc round robin load balancer and availability connector.

## Features

* Query k8s for pods to connect to by using the dynamic ip addresses assigned in the k8s services;
* Connect to the full set of available pods. If there are multiple pods the connection availability goes up;
* Cache connections to create connection pool from which connections are handed out;
* Automatic refresh of connection pool to account for autoscaling environments, pod updates and pod crashes;
* Stays connected to pods even when service is removed (Does not reconnect when pods are restarted).

## Versioning

Since the k8s API is used, the minor version in the release number has been used to indicate the k8s minor version used to build & test with.

## k8s compatibility

### Release 1.17.x

Release 1.17.x have been tested with k8s 1.13. to 1.17 and no issues have been observed.

### Release 1.18.x

Release 1.18.x have been tested with k8s 1.14. to 1.18 and no issues have been observed.

## Project state

The project has been used in several dozen code bases before being made publicly available, thus adding some real world use and test experience.

## Usage

To use the package, the developer has to implement the interface `GrpcKubeBalancer`.
By passing the interface implementation to the `Connect` function, the connection management process will start. `Connect` can be called multiple times for different connections. The package handles the connections internally in a map in which the key is the service name. THe input service name expected is the servicename in FQDN notation including connection port (eg `abc.ns.svc.local:10000`).

### Usage example

Implement in the grpc interface the following function:

```proto3
   rpc Ping(Pong) returns (Pong) {}

   message Pong {
       string Pong = 1; // Pong can be any indepth message for further in depth considerations like resend on failure
   }
```

Implement the interface with the call back functions:

```go
type i struct{}
var iFunctions=&i{} // This is send in every call to the kube-grpc package

// NewGrpcClient - Implementation requirement for connection pool
func (*i) NewGrpcClient(conn *grpc.ClientConn) (interface{}, error) {
	return someGrpc.NewSomeGrpcClient(conn), nil
}

// Ping - Implementation requirement for connection pool
func (*i) Ping(grpcConnection interface{}) error {
	// Cast interface to connection
	pinger := grpcConnection.(someGrpc.SomeGrpcClient)
	// Ping - Can be any function in the grpc interface
	_, err := pinger.Ping(context.Background(), &someGrpc.Pong{})
	return err
}
```

Use the connection package by using the `Connect` function:

```go
func someGrpcConnect() (someGrpc.someGrpcClient, error) {
	conn, err := kubegrpc.Connect("service-address.namespace.svc.cluster.local:portnumber", iFunctions)
	if err != nil {
		return nil, err
	}
	return conn.(someGrpc.SomeGrpcClient), nil
}
```

### Requirements

The package requires access to k8s to get the services from.

### GKE requirements for clusters 1.14.10-gke.27 and up (and maybe down)

The code has been tested on a running GKE cluster upgraded from 1.13 (or maybe older). This had a different set of rol bindings.

The following gke requirements are there:

* Compute account needs at minimum view rights on the kubernetes engine (see IAM in gcloud).
* The ClusterRoleBinding might be missing on a fresh install leading to an error (`services is forbidden: User "system:serviceaccount:{YOURNAMESPACE}:default" cannot list resource "services" in API group "" in the namespace "{YOURNAMESPACE}"` or similar) when connecting to the k8s for the list of services and pods.

To solve this second issue, the below ClusterRoleBinding can be used:

```yaml
apiVersion: rbac.authorization.k8s.io/v1beta1
kind: ClusterRoleBinding
metadata:
 name: cluster-admin-binding
subjects:
- kind: User
  name: system:serviceaccount:{YOURNAMESPACE}:default
  apiGroup: ""
roleRef:
  kind: ClusterRole
  name: cluster-admin
  apiGroup: ""
```

A more narrow setup with just read rights should also be sufficient (samples are welcome).

## Performance

The use of a lookup in a map to get the connection is slower than just connecting to a grpc interface without using this package. However in any reasonable size scenario, a service probably uses only a few other services, thus creating a map with a very limited set of keys. Also the number of targets to connect is most likely low (<10 replicas), thus leading to a very limited overhead.

## Known limitations

### Pods do restart or crash (servers crash)

When pods restart, the code reconnects and updates the build in connection caches, however to limit overhead of checks this is only done every X seconds, which leaves a gap in which requests might go nowhere. Since this is also the case in a standard grpc connection setup, this package does not resend events or intercept events (Service meshes are your help for that, however see chapter 'Istio does not play nice' for the bad news).

### Istio does not play nice

As far as is known, this code does not play nice with Istio (and probably other service meshes). The problem is that this code connects to the POD IP by lack of DNS names for the pods. Istio uses DNS service based names to build up its service mesh and does not allow connections to other ips outside of that. This is for example also observed with Redis clustering (which also connects to IPs).
The hope is that either Istio becomes smarter (recognizes the IP as part of a defined service and thus starts allowing connections), or k8s to start adding DNS names for the pods (and Istio using this kind of feature).

The obeservation of the developer is however that Istio (1.0.4) is not capable of refreshing the list of pod ips associated with a service correctly, leading to having to restart pods unexpectedly anyway, so not a lot is lost with not being able to use Istio (at the moment).

## License

The MIT license has been used for this code.
