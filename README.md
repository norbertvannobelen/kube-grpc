# kube-grpc

Kubernetes grpc round robin load balancer and availability connector.

## Features

* Query k8s for pods to connect to by using the dynamic ip addresses assigned in the k8s services;
* Connect to the full set of available pods. If there are multiple pods the connection availability goes up;
* Cache connections to create connection pool from which connections are handed out;
* Automatic refresh of connection pool to account for autoscaling environments, pod updates and pod crashes.

## Project state

The project has been used in several dozen code bases before being made publicly available, thus adding some real world use and test experience.

## Usage

To use the package, the developer has to implement the interface `GrpcKubeBalancer`. 
By passing the interface implementation to the `Connect` function, the connection management process will start. `Connect` can be called multiple times for different connections. The package handles the connections internally in a map in which the key is the service name. THe input service name expected is the servicename in FQDN notation including connection port (eg `abc.ns.svc.local:10000`).

### Requirements

The package requires access to k8s to get the services from.

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