module github.com/agent-platform/poc

go 1.22

require (
	sigs.k8s.io/controller-runtime v0.17.6
	k8s.io/api v0.29.6
	k8s.io/apimachinery v0.29.6
	k8s.io/client-go v0.29.6
	github.com/authzed/authzed-go v1.9.0
	github.com/authzed/grpcutil v0.0.0-20240101215309-a6b07315de0d
	google.golang.org/grpc v1.68.0
	github.com/spiffe/go-spiffe/v2 v2.1.6
	github.com/lestrrat-go/jwx/v2 v2.0.21
)
