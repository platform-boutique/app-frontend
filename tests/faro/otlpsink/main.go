// Command otlpsink is a throwaway OTLP/gRPC trace receiver for tests/faro/run.sh. It accepts
// ExportTraceServiceRequest and prints one JSON line per span (trace id, span id, parent span id,
// name, service) to stdout, nothing else. Not part of the shop and not built into the image.
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"

	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
)

type server struct {
	coltrace.UnimplementedTraceServiceServer
}

func (server) Export(_ context.Context, req *coltrace.ExportTraceServiceRequest) (*coltrace.ExportTraceServiceResponse, error) {
	enc := json.NewEncoder(os.Stdout)
	for _, rs := range req.GetResourceSpans() {
		service := ""
		for _, kv := range rs.GetResource().GetAttributes() {
			if kv.GetKey() == "service.name" {
				service = kv.GetValue().GetStringValue()
			}
		}
		for _, ss := range rs.GetScopeSpans() {
			for _, sp := range ss.GetSpans() {
				_ = enc.Encode(map[string]string{
					"service": service, "name": sp.GetName(),
					"traceId": hex.EncodeToString(sp.GetTraceId()), "spanId": hex.EncodeToString(sp.GetSpanId()),
					"parentSpanId": hex.EncodeToString(sp.GetParentSpanId()),
				})
			}
		}
	}
	return &coltrace.ExportTraceServiceResponse{}, nil
}

func main() {
	addr := ":4317"
	if len(os.Args) > 1 {
		addr = os.Args[1]
	}
	lis, err := net.Listen("tcp", addr) // #nosec G102 -- throwaway test sink; it runs in a container and must be reachable on the private docker network
	if err != nil {
		log.Fatal(err)
	}
	s := grpc.NewServer()
	coltrace.RegisterTraceServiceServer(s, server{})
	fmt.Fprintln(os.Stderr, "otlpsink listening on", addr)
	log.Fatal(s.Serve(lis))
}
